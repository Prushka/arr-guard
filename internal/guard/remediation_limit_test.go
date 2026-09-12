package guard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
)

func seedRemediationAttempts(t *testing.T, f *safetyFixture, attempts int) {
	t.Helper()
	for _, key := range retryKeys(f.client.Kind(), f.file, []int{10, 12}) {
		for range attempts {
			if _, err := f.service.state.Increment(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRemediationLimitPreservesImportedFiles(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, origin := range []string{"automatic", "interactive", "guardSearch", "queue", "noHistory", "alreadyFailed", "unavailable"} {
			for _, attempts := range []int{6, 7} {
				for _, mode := range []string{"scan", "webhook", "dryScan"} {
					t.Run(kind+"/"+origin+"/"+strconv.Itoa(attempts)+"/"+mode, func(t *testing.T) {
						f := historySearchFixture(t, kind, "Rss")
						f.service.config.MaxAttempts = 6
						f.autoRedownload = origin != "guardSearch"
						switch origin {
						case "interactive":
							f.history[1].Data["releaseSource"] = "InteractiveSearch"
							f.interactiveRedownload = true
						case "queue":
							f.queue = []arr.QueueRecord{{ID: 7, SeriesID: 3, MovieID: 3, EpisodeID: 10, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}}
						case "noHistory":
							f.history = nil
						case "alreadyFailed":
							f.history = append(f.history, arr.HistoryRecord{ID: 3, SeriesID: 3, MovieID: 3, DownloadID: "origin", EventType: "downloadFailed"})
						case "unavailable":
							f.configResponse = map[string]bool{}
							f.fail = "GET /api/v3/history/series"
							if kind == "radarr" {
								f.fail = "GET /api/v3/history/movie"
							}
						}
						seedRemediationAttempts(t, f, attempts)
						before, err := os.ReadFile(f.service.state.path)
						if err != nil {
							t.Fatal(err)
						}
						if mode == "dryScan" {
							f.service.config.DryRun = true
							f.client.EnforceReadOnly()
						}
						var logs bytes.Buffer
						f.service.log = slog.New(slog.NewTextHandler(&logs, nil))
						f.before = func(r *http.Request) {
							if strings.Contains(r.URL.Path, "/history") || strings.Contains(r.URL.Path, "/queue") || strings.Contains(r.URL.Path, "/config/") {
								t.Error("exhausted remediation reached origin preflight")
							}
						}
						if mode == "webhook" {
							payload := arr.WebhookPayload{EventType: "Download", DownloadID: "origin", EpisodeFile: &arr.WebhookFile{ID: 17}, MovieFile: &arr.WebhookFile{ID: 17}}
							var wg sync.WaitGroup
							for range 8 {
								wg.Go(func() {
									if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
										t.Error(err)
									}
								})
							}
							wg.Wait()
						} else {
							for range 2 {
								if err := f.service.Audit(t.Context()); err != nil {
									t.Fatal(err)
								}
							}
						}
						after, err := os.ReadFile(f.service.state.path)
						if err != nil || !bytes.Equal(before, after) {
							t.Fatal("exhausted remediation wrote retry state")
						}
						media, err := os.ReadFile(f.file.Path)
						if err != nil || string(media) != "fixture media" || f.deleted || len(f.mutations) != 0 || len(f.commands) != 0 || len(f.service.state.Pending()) != 0 {
							t.Fatal("exhausted remediation changed Arr or media")
						}
						if !strings.Contains(logs.String(), "subtitle remediation skipped; attempt limit reached") || !strings.Contains(logs.String(), "attempts="+strconv.Itoa(attempts)) {
							t.Fatal("exhausted file was not logged as a policy skip")
						}
					})
				}
			}
		}
	}
}

func TestRemediationLimitSurvivesReplacementAndRestart(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, automatic := range []bool{false, true} {
			t.Run(kind+"/automatic="+strconv.FormatBool(automatic), func(t *testing.T) {
				f := historySearchFixture(t, kind, "Rss")
				f.service.config.MaxAttempts = 6
				f.autoRedownload = automatic
				seedRemediationAttempts(t, f, 5)
				if err := f.apply(t.Context()); err != nil {
					t.Fatal(err)
				}
				want := 3
				if automatic {
					want = 2
				}
				if len(f.mutations) != want {
					t.Fatalf("last allowed attempt did not finish: %v", f.mutations)
				}
				store, err := LoadStateStore(f.service.state.path)
				if err != nil {
					t.Fatal(err)
				}
				f.service.state = store
				// Simulate a later Arr import assigning a new file ID to the same
				// movie/episodes. It must not evade the persisted subject budget.
				f.file.ID, f.deleted = 18, false
				for i := range f.episodes {
					f.episodes[i].EpisodeFileID = 18
				}
				f.handle = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "file/18") {
						if err := json.NewEncoder(w).Encode(f.file); err != nil {
							t.Error(err)
						}
						return true
					}
					return false
				}
				before, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.apply(t.Context()); err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(store.path)
				if err != nil || !bytes.Equal(before, after) || len(f.mutations) != want || f.deleted {
					t.Fatal("replacement or restart bypassed the remediation cap")
				}
				for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
					if store.Attempts(key) != 6 {
						t.Fatal("exhausted counter changed")
					}
				}
			})
		}
	}
}

func TestRemediationLimitAllowsValidFileReset(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := newSafetyFixture(t, kind)
		seedRemediationAttempts(t, f, f.service.config.MaxAttempts+1)
		f.validation.Valid = true
		if err := f.apply(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, key := range retryKeys(kind, f.file, []int{10, 12}) {
			if f.service.state.Attempts(key) != 0 {
				t.Fatal("valid file could not reset exhausted counter")
			}
		}
		if len(f.mutations) != 0 {
			t.Fatal("valid file triggered Arr mutation")
		}
	}
}
