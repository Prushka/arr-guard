package guard

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
)

func historySearchFixture(t *testing.T, kind, source string) *safetyFixture {
	t.Helper()
	f := newSafetyFixture(t, kind)
	f.history = []arr.HistoryRecord{
		{ID: 2, SeriesID: 3, MovieID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}},
		{ID: 1, SeriesID: 3, MovieID: 3, DownloadID: "origin", EventType: "grabbed", Data: map[string]string{"releaseSource": source}},
	}
	return f
}

func TestHistoryRemediationSelectsOneSearchOwner(t *testing.T) {
	cases := []struct {
		name, source                string
		auto, interactive, wantAuto bool
	}{
		{"disabled", "Rss", false, false, false},
		{"mainDisabledInteractiveEnabled", "InteractiveSearch", false, true, false},
		{"rss", "Rss", true, false, true},
		{"automaticSearch", "Search", true, false, true},
		{"userSearch", "UserInvokedSearch", true, false, true},
		{"interactiveDisabled", "InteractiveSearch", true, false, false},
		{"interactiveEnabled", "InteractiveSearch", true, true, true},
		{"numericInteractive", "4", true, false, false},
		{"numericSearch", "2", true, false, true},
		{"unknown", "", true, false, true},
		{"unrecognizedDefaultsToUnknown", "unrecognized", true, false, true},
		{"enumIsCaseSensitive", "interactivesearch", true, false, true},
	}
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f := historySearchFixture(t, kind, tc.source)
				f.autoRedownload, f.interactiveRedownload = tc.auto, tc.interactive
				if err := f.apply(t.Context()); err != nil {
					t.Fatal(err)
				}
				resource := "episodefile"
				if kind == "radarr" {
					resource = "moviefile"
				}
				want := []string{"DELETE /api/v3/" + resource + "/17", "POST /api/v3/history/failed/1"}
				if !tc.wantAuto {
					want = append(want, "POST /api/v3/command")
				}
				if !slices.Equal(f.mutations, want) {
					t.Fatalf("actions=%v want=%v", f.mutations, want)
				}
				if len(f.commands) > 0 {
					command := f.commands[0]
					if kind == "sonarr" && (command.Name != "EpisodeSearch" || !slices.Equal(command.EpisodeIDs, []int{10, 12})) {
						t.Fatalf("wrong episode targets: %+v", command)
					}
					if kind == "radarr" && (command.Name != "MoviesSearch" || !slices.Equal(command.MovieIDs, []int{3})) {
						t.Fatalf("wrong movie target: %+v", command)
					}
				}
				if len(f.service.state.Pending()) != 0 {
					t.Fatal("completed remediation remains pending")
				}
				reloaded, err := LoadStateStore(f.service.state.path)
				if err != nil {
					t.Fatal(err)
				}
				if !reloaded.state.Completed[operationKey(kind, 17)] {
					t.Fatal("completion was not persisted")
				}
				for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
					if reloaded.Attempts(key) != 1 {
						t.Fatal("remediation attempt was not recorded")
					}
				}
			})
		}
	}
}

func TestHistorySearchUsesSettingsRecheckedBeforeFailure(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, initial := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "enabledDuringDelete", true: "disabledDuringDelete"}[initial], func(t *testing.T) {
				f := historySearchFixture(t, kind, "Rss")
				f.autoRedownload = initial
				f.before = func(r *http.Request) {
					if r.Method == http.MethodDelete {
						f.autoRedownload = !initial
					}
					if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "history/failed") {
						op := f.service.state.Pending()[operationKey(kind, 17)]
						if op.Phase != "origin-requested" || op.AutomaticSearch == initial {
							t.Error("latest search owner not journaled before history failure")
						}
					}
				}
				if err := f.apply(t.Context()); err != nil {
					t.Fatal(err)
				}
				wantCommands := 0
				if initial {
					wantCommands = 1
				}
				if len(f.commands) != wantCommands {
					t.Fatalf("commands=%d want=%d", len(f.commands), wantCommands)
				}
			})
		}
	}
}

func TestHistorySearchUnknownSettingsStopBeforeDeletion(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, response := range []any{map[string]bool{}, map[string]any{"autoRedownloadFailed": nil}, map[string]bool{"autoRedownloadFailed": true}, map[string]string{"autoRedownloadFailed": "true"}} {
			f := historySearchFixture(t, kind, "InteractiveSearch")
			f.configResponse = response
			if err := f.apply(t.Context()); err == nil {
				t.Fatal("unknown effective search policy accepted")
			}
			if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
				t.Fatal("unknown policy caused mutation")
			}
		}
	}
}

func TestHistorySearchDoesNotRequireIrrelevantInteractiveSetting(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		f := historySearchFixture(t, "sonarr", "Rss")
		f.configResponse = map[string]bool{"autoRedownloadFailed": automatic}
		if err := f.apply(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistorySearchFailureNeverFallsBackOrReplays(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, failure := range []string{"history", "configAfterDelete", "persistenceBeforeHistory", "persistenceAfterHistory"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				f := historySearchFixture(t, kind, "Rss")
				f.autoRedownload = true
				f.before = func(r *http.Request) {
					if failure == "persistenceAfterHistory" && r.Method == http.MethodPost && strings.Contains(r.URL.Path, "history/failed") {
						f.service.state.path = f.file.Path + "/state.json"
					}
					if failure == "history" {
						f.fail = "POST /api/v3/history/failed/1"
					}
					if f.deleted && r.URL.Path == "/api/v3/config/downloadclient" {
						if failure == "configAfterDelete" {
							f.fail = "GET /api/v3/config/downloadclient"
						}
						if failure == "persistenceBeforeHistory" {
							// Break only the disposable state destination immediately before
							// persisting origin-requested; the durable deleted phase remains.
							f.service.state.path = f.file.Path + "/state.json"
						}
					}
				}
				originalStatePath := f.service.state.path
				if err := f.apply(t.Context()); err == nil {
					t.Fatal("expected interrupted remediation")
				}
				if len(f.commands) != 0 {
					t.Fatal("uncertain outcome caused a fallback search")
				}
				calls := len(f.mutations)
				reloaded, err := LoadStateStore(originalStatePath)
				if err != nil {
					t.Fatal(err)
				}
				op := reloaded.Pending()[operationKey(kind, 17)]
				wantPhase := "deleted"
				if failure == "history" || failure == "persistenceAfterHistory" {
					wantPhase = "origin-requested"
				}
				if op.Phase != wantPhase || !op.AutomaticSearch {
					t.Fatalf("incorrect durable recovery: %+v", op)
				}
				f.service.state = reloaded
				if err := f.service.CleanupPending(t.Context()); err == nil {
					t.Fatal("lost reconciliation requirement")
				}
				_ = f.apply(t.Context())
				if len(f.mutations) != calls {
					t.Fatal("restart replayed uncertain operation")
				}
			})
		}
	}
}

func TestAutomaticHistorySearchCompletesLastAllowedAttempt(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := historySearchFixture(t, kind, "Rss")
		f.autoRedownload = true
		for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
			for i := 0; i < f.service.config.MaxAttempts-1; i++ {
				if _, err := f.service.state.Increment(key); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := f.apply(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(f.mutations) != 2 || len(f.commands) != 0 || len(f.service.state.Pending()) != 0 {
			t.Fatalf("remediation at cap: %v", f.mutations)
		}
		for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
			if f.service.state.Attempts(key) != f.service.config.MaxAttempts {
				t.Fatal("last allowed attempt was not counted")
			}
		}
	}
}

func TestQueueAndAlreadyFailedOriginsStillUseGuardSearch(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, queued := range []bool{false, true} {
			f := historySearchFixture(t, kind, "Rss")
			f.autoRedownload = true
			if queued {
				f.queue = []arr.QueueRecord{{ID: 7, MovieID: 3, SeriesID: 3, EpisodeID: 10, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}}
			} else {
				f.history = append(f.history, arr.HistoryRecord{ID: 3, MovieID: 3, SeriesID: 3, DownloadID: "origin", EventType: "downloadFailed"})
			}
			if err := f.apply(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.commands) != 1 {
				t.Fatal("no new automatic search is triggered; guard must search")
			}
			for _, mutation := range f.mutations {
				if strings.Contains(mutation, "history/failed") {
					t.Fatal("unexpected history failure")
				}
			}
		}
	}
}

func TestAutomaticHistorySearchDryRunPreservesStateAndMedia(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := historySearchFixture(t, kind, "Rss")
		f.autoRedownload = true
		f.service.config.DryRun = true
		f.client.EnforceReadOnly()
		if _, err := f.service.state.Increment("existing"); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(f.service.state.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.apply(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(f.service.state.path)
		if err != nil || string(before) != string(after) || len(f.mutations) != 0 {
			t.Fatal("dry run changed state or Arr")
		}
		media, err := os.ReadFile(f.file.Path)
		if err != nil || string(media) != "fixture media" {
			t.Fatal("media was modified")
		}
	}
}

func TestConcurrentAutomaticHistoryWebhooksOnlyRemediateOnce(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := historySearchFixture(t, kind, "Rss")
		f.autoRedownload = true
		payload := arr.WebhookPayload{EventType: "Download", DownloadID: "origin"}
		if kind == "sonarr" {
			payload.EpisodeFile = &arr.WebhookFile{ID: 17}
		} else {
			payload.MovieFile = &arr.WebhookFile{ID: 17}
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Go(func() {
				if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if len(f.mutations) != 2 || len(f.commands) != 0 {
			t.Fatalf("duplicate automatic remediation: %v", f.mutations)
		}
	}
}
