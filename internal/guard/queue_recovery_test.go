package guard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
)

func pendingQueueItem(id, episode int, origin, reason string) arr.QueueRecord {
	return arr.QueueRecord{ID: id, EpisodeID: episode, SeriesID: 3, MovieID: 3, DownloadID: origin, Status: "completed", TrackedDownloadState: "importPending", StatusMessages: []arr.QueueStatusMessage{{Messages: []string{reason}}}}
}

func newPartialQueueFixture(t *testing.T) *safetyFixture {
	t.Helper()
	f := newSafetyFixture(t, "sonarr")
	f.episodes = []arr.Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 17}, {ID: 12, SeriesID: 3, EpisodeFileID: 17}, {ID: 14, SeriesID: 3}, {ID: 16, SeriesID: 3}}
	for i, episode := range f.episodes {
		f.queue = append(f.queue, pendingQueueItem(7+i, episode.ID, "pack", "No files found are eligible for import in fixture"))
	}
	return f
}

func TestQueueWaitingImportReasons(t *testing.T) {
	reasons := []string{
		"Not an upgrade for existing episode file(s)",
		"No files found are eligible for import in fixture",
		"Not a quality revision upgrade for existing episode file(s)",
		"Unable to parse file",
		"BDRip-1080p was unexpected considering the release was grabbed as HDTV-1080p",
		"Unable to determine if file is a sample",
	}
	for _, reason := range reasons {
		q := pendingQueueItem(7, 10, "pack", strings.ToUpper(reason))
		if !q.NeedsImportRecovery() {
			t.Errorf("reason ignored: %s", reason)
		}
		q.StatusMessages = []arr.QueueStatusMessage{{Title: reason}}
		if !q.NeedsImportRecovery() {
			t.Errorf("title ignored: %s", reason)
		}
		for _, state := range []string{"importing", "imported", "failedPending", "unknown"} {
			q.TrackedDownloadState = state
			if q.NeedsImportRecovery() {
				t.Errorf("active/unknown state accepted: %s", state)
			}
		}
		q.TrackedDownloadState, q.Status = "importPending", "downloading"
		if q.NeedsImportRecovery() {
			t.Error("incomplete download accepted")
		}
	}
	for _, reason := range []string{"", "Permission denied", "Path does not exist", "Some unrelated quality message", "Some file named Unable to parse file.mkv"} {
		if pendingQueueItem(7, 10, "pack", reason).NeedsImportRecovery() {
			t.Errorf("unknown reason accepted: %s", reason)
		}
	}
}

func TestPartialQueueRecoversOnlyMissingEpisodes(t *testing.T) {
	f := newPartialQueueFixture(t)
	for _, id := range []int{10, 12} {
		for range 3 {
			if _, err := f.service.state.Increment(retryKeys("sonarr", f.file, []int{id})[0]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.mutations, []string{"DELETE /api/v3/queue/7", "POST /api/v3/command"}) || len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{14, 16}) {
		t.Fatalf("wrong partial actions: %v %v", f.mutations, f.commands)
	}
	for _, id := range []int{10, 12, 14, 16} {
		want := 1
		if id == 10 || id == 12 {
			want = 3
		}
		if f.service.state.Attempts(retryKeys("sonarr", f.file, []int{id})[0]) != want {
			t.Fatal("wrong per-episode retry count")
		}
	}
	if len(f.service.state.Pending()) != 0 || f.deleted {
		t.Fatal("partial cleanup changed library or left a journal")
	}
	if _, err := os.Stat(f.file.Path); err != nil {
		t.Fatal("fixture media changed")
	}
}

func TestQueueExistingMediaOnlyCleansUpWithoutSearchOrRetries(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			f := newSafetyFixture(t, kind)
			f.queue = []arr.QueueRecord{pendingQueueItem(7, 10, "pack", "Not an upgrade for existing episode")}
			for range 3 {
				for _, key := range retryKeys(kind, f.file, []int{10}) {
					if _, err := f.service.state.Increment(key); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := len(f.service.state.state.Attempts)
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 1 || len(f.commands) != 0 || len(f.service.state.state.Attempts) != before || f.deleted {
				t.Fatal("existing media cleanup searched or changed files")
			}
		})
	}
}

func TestQueueMissingMovieSearchAndDryRun(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newSafetyFixture(t, "radarr")
		f.deleted = true
		f.service.config.DryRun = dry
		f.queue = []arr.QueueRecord{pendingQueueItem(7, 0, "pack", "Unable to determine if file is a sample")}
		if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
			t.Fatal(err)
		}
		if dry {
			if len(f.mutations)+len(f.service.state.state.Attempts)+len(f.service.state.Pending()) != 0 {
				t.Fatal("dry run mutated")
			}
			if _, err := os.Stat(f.service.state.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("dry run wrote state")
			}
		} else if len(f.commands) != 1 || !slices.Equal(f.commands[0].MovieIDs, []int{3}) {
			t.Fatal("missing movie was not searched")
		}
	}
}

func TestQueuePriorImportAlwaysRequestsClientRemoval(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"existing", "missing", "dryRun"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				f := newSafetyFixture(t, kind)
				f.deleted = scenario == "missing"
				f.service.config.DryRun = scenario == "dryRun"
				f.queue = []arr.QueueRecord{pendingQueueItem(7, 10, "pack", "No files found are eligible for import")}
				f.history = []arr.HistoryRecord{
					{ID: 1, SeriesID: 3, MovieID: 3, EpisodeID: 10, DownloadID: "pack", EventType: "grabbed"},
					{ID: 2, SeriesID: 3, MovieID: 3, EpisodeID: 10, DownloadID: "pack", EventType: "downloadFolderImported"},
				}
				if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
					t.Fatal(err)
				}
				want := []string{"DELETE /api/v3/queue/7"}
				if scenario == "missing" {
					want = append(want, "POST /api/v3/command")
				}
				if scenario == "dryRun" {
					want = nil
					if len(f.service.state.state.Attempts)+len(f.service.state.Pending()) != 0 {
						t.Fatal("dry run changed retry state")
					}
					if _, err := os.Stat(f.service.state.path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("dry run wrote state")
					}
				}
				// The HTTP fixture requires removeFromClient=true for every DELETE.
				if !slices.Equal(f.mutations, want) || f.deleted != (scenario == "missing") {
					t.Fatalf("prior import changed removal/search policy: %v", f.mutations)
				}
			})
		}
	}
}

func TestQueuePartialImportedRowsAndHistoryCompleteTheMapping(t *testing.T) {
	f := newPartialQueueFixture(t)
	f.queue = f.queue[:1]
	// A visible queue row can omit episodes still present in grabbed history.
	for i, episode := range f.episodes {
		f.history = append(f.history, arr.HistoryRecord{ID: i + 1, SeriesID: 3, EpisodeID: episode.ID, DownloadID: "pack", EventType: "grabbed"})
	}
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{14, 16}) {
		t.Fatal("history-only targets were lost")
	}
	f = newPartialQueueFixture(t)
	f.queue[0].TrackedDownloadState = "imported"
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{14, 16}) {
		t.Fatal("imported sibling blocked partial recovery")
	}
}

func TestQueueRecoveryRefusesUnreliablePreflight(t *testing.T) {
	for _, failure := range []string{"active", "ordinaryPending", "missingImportedFile", "crossSubject", "crossHistory", "missingHistory", "historyFailure", "missingEpisode", "missingMapping", "reassignedID", "changedQueue", "canceled", "retryLimit", "journalFailure"} {
		t.Run(failure, func(t *testing.T) {
			f := newPartialQueueFixture(t)
			item := f.queue[0]
			ctx := t.Context()
			switch failure {
			case "active":
				f.queue[1].Status = "downloading"
			case "ordinaryPending":
				f.queue[1].StatusMessages = nil
			case "missingImportedFile":
				f.queue[2].TrackedDownloadState = "imported"
			case "crossSubject":
				f.queue[1].SeriesID = 4
			case "crossHistory":
				f.history = []arr.HistoryRecord{{ID: 1, SeriesID: 4, EpisodeID: 14, DownloadID: "pack", EventType: "grabbed"}}
			case "missingHistory":
				f.emptyQueueHistory = true
			case "historyFailure":
				f.fail = "GET /api/v3/history"
			case "missingEpisode":
				f.episodes = f.episodes[:3]
			case "missingMapping":
				f.queue[1].EpisodeID = 0
			case "reassignedID":
				f.queue[0].DownloadID = "another"
			case "changedQueue":
				reads := 0
				f.before = func(r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue" {
						reads++
						if reads == 2 {
							f.queue[2].TrackedDownloadState = "importing"
						}
					}
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "retryLimit":
				for range 3 {
					if _, err := f.service.state.Increment(retryKeys("sonarr", f.file, []int{14})[0]); err != nil {
						t.Fatal(err)
					}
				}
			case "journalFailure":
				f.service.state.writeErr = errors.New("injected failed persistence")
			}
			if err := f.service.recoverBlockedQueueItem(ctx, f.client, item); err == nil {
				t.Fatal("unsafe preflight passed")
			}
			if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
				t.Fatal("preflight failure mutated Arr or started a journal")
			}
		})
	}
}

func TestQueueHistoryCanRecoverOnFreshRead(t *testing.T) {
	f := newPartialQueueFixture(t)
	f.emptyQueueHistory = true
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err == nil || !canRetryProcessing(err) {
		t.Fatal("missing history must defer")
	}
	f.emptyQueueHistory = false
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
}

func TestQueueHistoryResolvesUnparsedSubjectAndEpisodes(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := newSafetyFixture(t, kind)
		f.deleted = true
		q := pendingQueueItem(7, 0, "pack", "Unable to parse file")
		q.SeriesID, q.MovieID = 0, 0
		f.queue = []arr.QueueRecord{q}
		f.history = []arr.HistoryRecord{{ID: 1, SeriesID: 3, MovieID: 3, EpisodeID: 10, DownloadID: "pack", EventType: "grabbed"}}
		if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
			t.Fatal(err)
		}
		if len(f.commands) != 1 {
			t.Fatal("history-resolved target was not recovered")
		}
		if kind == "sonarr" && !slices.Equal(f.commands[0].EpisodeIDs, []int{10}) {
			t.Fatal("wrong resolved episode search")
		}
		if kind == "radarr" && !slices.Equal(f.commands[0].MovieIDs, []int{3}) {
			t.Fatal("wrong resolved movie search")
		}
	}
}

func TestQueueDuplicateDownloadsDoNotRepeatSearch(t *testing.T) {
	f := newPartialQueueFixture(t)
	copyOfQueue := append([]arr.QueueRecord(nil), f.queue...)
	for _, q := range copyOfQueue {
		q.ID += 100
		q.DownloadID = "duplicate"
		f.queue = append(f.queue, q)
	}
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 3 || len(f.commands) != 1 || len(f.queue) != 0 {
		t.Fatalf("duplicate searches: %v", f.mutations)
	}
	if f.service.state.Attempts(retryKeys("sonarr", f.file, []int{14})[0]) != 1 {
		t.Fatal("duplicate download consumed extra retry")
	}
}

func TestQueueActiveAlternativeAndRacingReplacement(t *testing.T) {
	for _, when := range []string{"before", "afterRemoval", "newLibraryFile"} {
		f := newPartialQueueFixture(t)
		alternative := pendingQueueItem(100, 14, "replacement", "")
		alternative.Status, alternative.TrackedDownloadState = "downloading", "downloading"
		if when == "before" {
			f.queue = append(f.queue, alternative)
		} else {
			f.before = func(r *http.Request) {
				if r.Method == http.MethodDelete {
					if when == "afterRemoval" {
						f.queue = append(f.queue, alternative)
					} else {
						f.episodes[2].EpisodeFileID = 99
					}
				}
			}
		}
		if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
			t.Fatal(err)
		}
		if len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{16}) {
			t.Fatal("existing replacement was searched again", when, f.commands)
		}
	}
}

func TestQueueUncertainMutationSurvivesRestartWithoutReplay(t *testing.T) {
	for _, failure := range []string{"DELETE /api/v3/queue/7", "POST /api/v3/command", "postRemovalRead"} {
		f := newPartialQueueFixture(t)
		item := f.queue[0]
		f.fail = failure
		if failure == "postRemovalRead" {
			f.before = func(r *http.Request) {
				if r.Method == http.MethodDelete {
					f.fail = "GET /api/v3/queue"
				}
			}
		}
		if err := f.service.recoverBlockedQueueItem(t.Context(), f.client, item); err == nil || canRetryProcessing(err) {
			t.Fatal("uncertain mutation became retryable")
		}
		pending := f.service.state.Pending()["sonarr:queue:pack"]
		if !slices.Equal(pending.EpisodeIDs, []int{10, 12, 14, 16}) {
			t.Fatal("journal lost partial operation intent")
		}
		if failure == "POST /api/v3/command" && !slices.Equal(pending.SearchEpisodeIDs, []int{14, 16}) {
			t.Fatal("journal lost search scope")
		}
		calls := len(f.mutations)
		state, err := LoadStateStore(f.service.state.path)
		if err != nil {
			t.Fatal(err)
		}
		f.service.state, f.fail, f.before = state, "", nil
		// A repeated delivery or changed queue cannot authorize mutation replay.
		f.queue = []arr.QueueRecord{item}
		_ = f.service.recoverBlockedQueueItem(t.Context(), f.client, item)
		_ = f.service.CleanupPending(t.Context())
		if len(f.mutations) != calls || len(state.Pending()) != 1 {
			t.Fatal("uncertain operation replayed")
		}
	}
}

func TestConcurrentPartialQueueRecoveryRunsOnce(t *testing.T) {
	f := newPartialQueueFixture(t)
	item := f.queue[0]
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := f.service.recoverBlockedQueueItem(t.Context(), f.client, item); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(f.mutations) != 2 || len(f.commands) != 1 {
		t.Fatal("concurrent recovery replayed", f.mutations)
	}
}

func TestQueueRecoveryHonorsOptInInScansAndServe(t *testing.T) {
	for _, scan := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			f := newSafetyFixture(t, "radarr")
			f.validation.Valid = true
			f.service.config.RecoverBlockedQueue = enabled
			f.queue = []arr.QueueRecord{pendingQueueItem(7, 0, "pack", "Not an upgrade for existing movie")}
			if scan {
				if err := f.service.Audit(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				f.service.runBlockedQueueScan(t.Context())
			}
			want := 0
			if enabled {
				want = 1
			}
			if len(f.mutations) != want || len(f.commands) != 0 {
				t.Fatal("queue opt-in or existing-file policy ignored")
			}
		}
	}
}
