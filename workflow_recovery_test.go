package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUnrelatedDirectoryActivityDoesNotBlockValidation(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, valid := range []bool{false, true} {
			f := newSafetyFixture(t, kind)
			f.validation.Valid = valid
			for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
				if _, err := f.service.state.Increment(key); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Dir(f.file.Path)
			if err := os.WriteFile(filepath.Join(dir, "OtherEpisode.en.srt"), []byte("unrelated subtitles"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "poster.jpg"), []byte("unrelated metadata"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(dir, time.Now(), f.validation.dirInfo.ModTime().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := f.apply(t.Context()); err != nil {
				t.Fatal(err)
			}
			if valid {
				if len(f.mutations) != 0 {
					t.Fatal("valid file mutated")
				}
				for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
					if f.service.state.Attempts(key) != 0 {
						t.Fatal("valid retry counters were not reset")
					}
				}
			} else if len(f.commands) != 1 || len(f.mutations) != 2 {
				t.Fatalf("unrelated directory entry blocked remediation: %v", f.mutations)
			}
		}
	}
}

func TestMatchingSubtitleChangesRemainProtected(t *testing.T) {
	for _, change := range []string{"add", "remove", "rename", "contents", "missingSnapshot"} {
		t.Run(change, func(t *testing.T) {
			f := newSafetyFixture(t, "radarr")
			dir := filepath.Dir(f.file.Path)
			sub := filepath.Join(dir, "fixture.en.srt")
			if err := os.WriteFile(sub, []byte("original subtitles"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			f.validation.sidecars, err = discoverExternalSubtitles(f.file.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "add":
				err = os.WriteFile(filepath.Join(dir, "fixture.fr.srt"), []byte("new subtitles"), 0o600)
			case "remove":
				err = os.Remove(sub)
			case "rename":
				err = os.Rename(sub, filepath.Join(dir, "fixture.fr.srt"))
			case "contents":
				err = os.WriteFile(sub, []byte("changed subtitle contents and size"), 0o600)
			case "missingSnapshot":
				f.validation.sidecarsChecked = false
			}
			if err != nil {
				t.Fatal(err)
			}
			// Even when the directory timestamp does not reveal the change, the
			// matching file snapshots must detect it.
			if err := os.Chtimes(dir, time.Now(), f.validation.dirInfo.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := f.apply(t.Context()); err == nil {
				t.Fatal("changed matching subtitle was accepted")
			}
			if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
				t.Fatal("changed subtitle caused mutation")
			}
		})
	}
}

func TestValidWebhooksNeedNoHistoryButInvalidOnesStillVerifyOrigin(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, valid := range []bool{true, false} {
			f := newSafetyFixture(t, kind)
			f.validation.Valid = valid
			historyReads := 0
			f.before = func(r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v3/history") {
					historyReads++
				}
			}
			payload := WebhookPayload{EventType: "Download", DownloadID: "origin"}
			if kind == "sonarr" {
				payload.EpisodeFile = &WebhookFile{ID: 17}
			} else {
				payload.MovieFile = &WebhookFile{ID: 17}
			}
			err := f.service.processWebhook(t.Context(), f.client, payload)
			if valid {
				if err != nil || historyReads != 0 {
					t.Fatalf("valid webhook depends on history: %v reads=%d", err, historyReads)
				}
			} else {
				if err == nil || !canRetryProcessing(err) || historyReads == 0 || len(f.mutations) != 0 {
					t.Fatalf("missing history not safely deferred: %v", err)
				}
				f.history = []HistoryRecord{{ID: 2, SeriesID: 3, MovieID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}, {ID: 1, SeriesID: 3, MovieID: 3, DownloadID: "origin", EventType: "grabbed"}}
				if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
					t.Fatal(err)
				}
				if len(f.mutations) != 3 {
					t.Fatalf("history visibility did not resume remediation: %v", f.mutations)
				}
			}
		}
	}
}

func TestPartialReplacementsOnlySearchRemainingEpisodes(t *testing.T) {
	for _, allReplaced := range []bool{false, true} {
		f := newSafetyFixture(t, "sonarr")
		f.replacementEpisodes = []Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 99}, {ID: 12, SeriesID: 3}}
		if allReplaced {
			f.replacementEpisodes[1].EpisodeFileID = 100
		}
		f.before = func(r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/v3/command" {
				op := f.service.state.Pending()[operationKey("sonarr", 17)]
				if !slices.Equal(op.SearchEpisodeIDs, []int{12}) || op.Phase != "search-requested" {
					t.Error("actual remaining targets were not journaled")
				}
			}
		}
		if err := f.apply(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(f.service.state.Pending()) != 0 {
			t.Fatal("completed remediation remained pending")
		}
		if allReplaced {
			if len(f.commands) != 0 {
				t.Fatal("searched despite all replacements being present")
			}
		} else if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{12}) {
			t.Fatalf("incorrect search: %+v", f.commands)
		}
	}
}

func TestMovieReplacementCompletesWithoutDuplicateSearch(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.replacementFiles = []MediaFile{{ID: 99, MovieID: 3}}
	if err := f.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 0 || len(f.service.state.Pending()) != 0 {
		t.Fatal("movie replacement left a search or unfinished operation")
	}
}

func TestQueueRemovalKeepsPreflightProtectionButFiltersLaterReplacements(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	f.deleted = true
	f.queue = []QueueRecord{{ID: 7, DownloadID: "origin", SeriesID: 3, EpisodeID: 10, Status: "completed", TrackedDownloadState: "importBlocked"}, {ID: 8, DownloadID: "origin", SeriesID: 3, EpisodeID: 12, Status: "completed", TrackedDownloadState: "importBlocked"}}
	f.before = func(r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue") {
			f.replacementEpisodes = []Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 99}, {ID: 12, SeriesID: 3}}
		}
	}
	if err := f.service.recoverBlockedQueueItem(t.Context(), f.client, f.queue[0]); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{12}) || len(f.service.state.Pending()) != 0 {
		t.Fatal("post-removal replacements blocked remaining search")
	}
	// A pack containing existing media before removal remains protected.
	f = newSafetyFixture(t, "sonarr")
	f.queue = []QueueRecord{{ID: 7, DownloadID: "origin", SeriesID: 3, EpisodeID: 10, Status: "completed", TrackedDownloadState: "importBlocked"}}
	if err := f.service.recoverBlockedQueueItem(t.Context(), f.client, f.queue[0]); err == nil || len(f.mutations) != 0 {
		t.Fatal("existing media did not protect queue removal")
	}
}

func TestMissingIdentityAndUncertainSearchStillNeedReview(t *testing.T) {
	for _, failure := range []string{"missingEpisode", "searchTimeout"} {
		f := newSafetyFixture(t, "sonarr")
		f.replacementEpisodes = []Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 99}, {ID: 12, SeriesID: 3}}
		if failure == "missingEpisode" {
			f.replacementEpisodes = f.replacementEpisodes[:1]
		} else {
			f.fail = "POST /api/v3/command"
		}
		err := f.apply(t.Context())
		if err == nil || canRetryProcessing(err) || len(f.service.state.Pending()) != 1 {
			t.Fatalf("uncertain search not protected: %v", err)
		}
		if failure == "searchTimeout" && !slices.Equal(f.service.state.Pending()[operationKey("sonarr", 17)].SearchEpisodeIDs, []int{12}) {
			t.Fatal("uncertain remaining search lost target IDs")
		}
		calls := len(f.mutations)
		_ = f.service.CleanupPending(t.Context())
		if len(f.mutations) != calls {
			t.Fatal("replayed uncertain search")
		}
	}
}

func TestScanRetriesFreshReadsAndPersistsDeferredWork(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		f := newSafetyFixture(t, "radarr")
		probes := 0
		f.service.probeFn = func(context.Context, string) (Validation, error) {
			probes++
			if persistent || probes == 1 {
				return Validation{}, errors.New("temporary inaccessible fixture")
			}
			return f.validation, nil
		}
		// Delivery dedup must not prevent a scan retry from revalidating this file.
		f.service.claimWebhook(operationKey("radarr", 17))
		err := f.service.auditFileWithRetries(t.Context(), f.client, f.file)
		if persistent {
			if err == nil || probes != scanReadAttempts || len(f.service.state.Webhooks()) != 1 || len(f.mutations) != 0 {
				t.Fatalf("deferred scan was lost: %v probes=%d", err, probes)
			}
			f.service.probeFn = func(context.Context, string) (Validation, error) { return f.validation, nil }
			f.service.handled = nil
			f.service.jobs = make(chan webhookJob, 1)
			f.service.dispatchStoredJobs()
			job := <-f.service.jobs
			if err := f.service.processWebhook(t.Context(), f.client, job.payload); err != nil {
				t.Fatal(err)
			}
			f.service.finishJob(job, nil)
			if len(f.service.state.Webhooks()) != 0 || len(f.commands) != 1 {
				t.Fatal("deferred scan did not finish in serve processing")
			}
		} else if err != nil || probes != 2 || len(f.commands) != 1 {
			t.Fatalf("scan did not recover: %v probes=%d", err, probes)
		}
	}
}

func TestScanDoesNotRetryMutationFailureOrWriteDryRunJobs(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.fail = "DELETE /api/v3/moviefile/17"
	err := f.service.auditFileWithRetries(t.Context(), f.client, f.file)
	if err == nil || len(f.mutations) != 1 || len(f.service.state.Webhooks()) != 0 {
		t.Fatal("scan retried a mutation or queued unsafe work")
	}
	f = newSafetyFixture(t, "radarr")
	f.service.config.DryRun = true
	f.service.probeFn = func(context.Context, string) (Validation, error) {
		return Validation{}, errors.New("temporary probe failure")
	}
	if err := f.service.auditFileWithRetries(t.Context(), f.client, f.file); err == nil {
		t.Fatal("missing deferred scan error")
	}
	if len(f.mutations) != 0 || len(f.service.state.Webhooks()) != 0 {
		t.Fatal("dry run wrote deferred state")
	}
	if _, err := os.Stat(f.service.state.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created state file")
	}
}

func TestDisappearingFilePreflightRecoversFromFreshRead(t *testing.T) {
	for _, finalRead := range []bool{false, true} {
		f := newSafetyFixture(t, "radarr")
		reads := 0
		f.before = func(r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/v3/moviefile/17" {
				reads++
				if reads == 1 && !finalRead || reads == 2 && finalRead {
					f.deleted = true // Simulate an external Arr change; no fixture file writes.
				}
			}
		}
		if err := f.service.auditFileWithRetries(t.Context(), f.client, f.file); err != nil {
			t.Fatalf("fresh read did not settle a disappeared candidate: %v", err)
		}
		if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 || len(f.service.state.Webhooks()) != 0 {
			t.Fatal("disappeared file mutated or left unresolved work")
		}
	}
}

func TestDelayedEpisodeMappingResumesWithoutReview(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	for i := range f.episodes {
		f.episodes[i].EpisodeFileID = 0
	}
	payload := WebhookPayload{EventType: "Download", EpisodeFile: &WebhookFile{ID: 17}}
	err := f.service.processWebhook(t.Context(), f.client, payload)
	if err == nil || !canRetryProcessing(err) || len(f.mutations) != 0 {
		t.Fatal("unassigned import did not wait safely for authoritative mapping")
	}
	for i := range f.episodes {
		f.episodes[i].EpisodeFileID = 17
	}
	if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{10, 12}) {
		t.Fatal("import did not resume with its complete episode mapping")
	}
}

func makeWebhookDue(t *testing.T, store *StateStore, key string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.updateLocked(func(next *State) {
		job := next.Webhooks[key]
		job.NextAttempt = time.Now().Add(-time.Second)
		next.Webhooks[key] = job
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTransientWebhooksRecoverBeyondFiveFailuresAndRestart(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	payload := WebhookPayload{EventType: "Download", MovieFile: &WebhookFile{ID: 17}}
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	key, _, err := storedWebhook("radarr", payload)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := f.service.state.FinishWebhook(key, deferProcessing(errors.New("temporary read"))); err != nil {
			t.Fatal(err)
		}
	}
	job := f.service.state.Webhooks()[key]
	if job.NeedsReview || job.Failures != maxWebhookBackoffFailures || time.Until(job.NextAttempt) > time.Hour || time.Until(job.NextAttempt) < 59*time.Minute {
		t.Fatal("transient retries not bounded to hourly backoff")
	}
	reloaded, err := LoadStateStore(f.service.state.path)
	if err != nil {
		t.Fatal(err)
	}
	f.service.state = reloaded
	makeWebhookDue(t, reloaded, key)
	f.service.jobs = make(chan webhookJob, 1)
	f.service.dispatchStoredJobs()
	if len(f.service.jobs) != 1 {
		t.Fatal("restarted transient job did not resume")
	}
	work := <-f.service.jobs
	if err := f.service.processWebhook(t.Context(), f.client, work.payload); err != nil {
		t.Fatal(err)
	}
	f.service.finishJob(work, nil)
	if len(reloaded.Webhooks()) != 0 || len(f.commands) != 1 {
		t.Fatal("recovered webhook did not complete")
	}
}

func TestPausedJobRedeliveryRechecksButDoesNotReplayUncertainMutation(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	payload := WebhookPayload{EventType: "Download", MovieFile: &WebhookFile{ID: 17}}
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	key, _, err := storedWebhook("radarr", payload)
	if err != nil {
		t.Fatal(err)
	}
	f.fail = "DELETE /api/v3/moviefile/17"
	err = f.service.processWebhook(t.Context(), f.client, payload)
	if err == nil || canRetryProcessing(err) {
		t.Fatal("mutation failure is retryable")
	}
	if err := f.service.state.FinishWebhook(key, err); err != nil {
		t.Fatal(err)
	}
	if !f.service.state.Webhooks()[key].NeedsReview {
		t.Fatal("uncertain job was not paused")
	}
	f.fail = ""
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	if f.service.state.Webhooks()[key].NeedsReview {
		t.Fatal("redelivery did not reactivate evaluation")
	}
	err = f.service.processWebhook(t.Context(), f.client, payload)
	if err == nil || canRetryProcessing(err) || len(f.mutations) != 1 {
		t.Fatal("redelivery replayed unresolved mutation")
	}
}

func TestUncertainSeriesOperationsRemainConservative(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	_, _, err := f.service.state.Begin("other", Operation{Kind: "sonarr", SubjectID: 3, FileID: 99, EpisodeIDs: []int{99}, DownloadID: "other-release", Phase: "origin-requested", AutomaticSearch: true}, []string{"sonarr:episodes:99"})
	if err != nil {
		t.Fatal(err)
	}
	err = f.apply(t.Context())
	if err == nil || canRetryProcessing(err) || len(f.mutations) != 0 {
		t.Fatal("uncertain shared-release side effects not protected")
	}
}

func TestRetryClassificationPreservesMutationBoundary(t *testing.T) {
	readFailure := &ArrHTTPError{Method: http.MethodGet, Status: http.StatusServiceUnavailable}
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"APIUnavailable", fmt.Errorf("preflight: %w", readFailure), true},
		{"APIRateLimit", &ArrHTTPError{Method: http.MethodGet, Status: http.StatusTooManyRequests}, true},
		{"APICredentials", &ArrHTTPError{Method: http.MethodGet, Status: http.StatusUnauthorized}, false},
		{"PostUnavailable", &ArrHTTPError{Method: http.MethodPost, Status: http.StatusServiceUnavailable}, false},
		{"ReadDeadline", context.DeadlineExceeded, true},
		{"TruncatedRead", fmt.Errorf("read Arr response: %w", io.ErrUnexpectedEOF), true},
		{"UncertainTruncatedRead", requireReconciliation(io.ErrUnexpectedEOF), false},
		{"Canceled", context.Canceled, false},
		{"UncertainMutationDeadline", requireReconciliation(context.DeadlineExceeded), false},
		{"ReadAfterDeletion", requireReconciliation(readFailure), false},
		{"DeferredCannotOverrideJournal", deferProcessing(requireReconciliation(readFailure)), false},
		{"UnknownCause", errors.New("unknown failure"), false},
		{"BatchStillHasSafeWork", errors.Join(requireReconciliation(readFailure), deferProcessing(errors.New("probe unavailable"))), true},
		{"BatchOnlyUnsafeWork", errors.Join(requireReconciliation(readFailure), errors.New("identity conflict")), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := canRetryProcessing(test.err); got != test.want {
				t.Fatalf("retry=%v want=%v", got, test.want)
			}
		})
	}
}

func TestLegacyExhaustedJobsResumeEvaluationWithJournalProtection(t *testing.T) {
	for _, pending := range []bool{false, true} {
		f := newSafetyFixture(t, "radarr")
		key, stored, err := storedWebhook("radarr", WebhookPayload{EventType: "Download", MovieFile: &WebhookFile{ID: 17}})
		if err != nil {
			t.Fatal(err)
		}
		stored.Failures = 5
		stored.NextAttempt = time.Now().Add(-time.Minute)
		legacy := State{Attempts: map[string]int{}, Webhooks: map[string]StoredWebhook{key: stored}}
		if pending {
			legacy.Operations = map[string]Operation{operationKey("radarr", 17): {Kind: "radarr", SubjectID: 3, FileID: 17, Phase: "delete-requested"}}
		}
		data, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "needsReview") {
			t.Fatal("fixture does not match the old state shape")
		}
		if err := os.WriteFile(f.service.state.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		f.service.state, err = LoadStateStore(f.service.state.path)
		if err != nil {
			t.Fatal(err)
		}
		f.service.jobs = make(chan webhookJob, 1)
		f.service.dispatchStoredJobs()
		if len(f.service.jobs) != 1 {
			t.Fatal("legacy job did not resume evaluation")
		}
		job := <-f.service.jobs
		err = f.service.processWebhook(t.Context(), f.client, job.payload)
		f.service.finishJob(job, err)
		if pending {
			if err == nil || !f.service.state.Webhooks()[key].NeedsReview || len(f.mutations) != 0 {
				t.Fatal("legacy job bypassed its uncertain journal")
			}
		} else if err != nil || len(f.commands) != 1 || len(f.service.state.Webhooks()) != 0 {
			t.Fatalf("safe legacy job did not finish: %v", err)
		}
	}
}
