package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/probe"
)

type safetyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f safetyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDryRunSerializesPreflightsWithoutMutations(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	f.service.config.DryRun = true
	var active, peak atomic.Int32
	f.client.SetTransport(safetyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		// A slow read makes overlap observable without changing fixture media.
		time.Sleep(5 * time.Millisecond)
		return http.DefaultTransport.RoundTrip(r)
	}))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			<-start
			if err := f.apply(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := peak.Load(); got != 1 {
		t.Fatalf("dry-run preflight reads overlapped: peak=%d", got)
	}
	if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 || len(f.service.state.state.Attempts) != 0 {
		t.Fatal("dry-run preflight changed Arr or retry state")
	}
}

func TestConcurrentWebhooksSerializeOriginHistoryReads(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	f.service.config.DryRun = true
	var active, peak, probes, mutations atomic.Int32
	f.service.probeFn = func(context.Context, string) (probe.Validation, error) {
		probes.Add(1)
		return f.validation, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "unexpected mutation", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/") {
			id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"))
			if err != nil {
				t.Error(err)
				return
			}
			file := f.file
			file.ID = id
			if err := json.NewEncoder(w).Encode(file); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/api/v3/episode" {
			var episodes []arr.Episode
			for id := 17; id < 25; id++ {
				episodes = append(episodes, arr.Episode{ID: id + 100, SeriesID: 3, EpisodeFileID: id, AirDate: "2025-01-01"})
			}
			if err := json.NewEncoder(w).Encode(episodes); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/api/v3/history/series" {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			time.Sleep(10 * time.Millisecond)
			http.Error(w, "injected busy server", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := testArrClient("sonarr", server.URL)
	var wg sync.WaitGroup
	for id := 17; id < 25; id++ {
		wg.Go(func() {
			payload := arr.WebhookPayload{EventType: "Download", DownloadID: "origin", Series: &arr.Series{ID: 3}, EpisodeFile: &arr.WebhookFile{ID: id}}
			if err := f.service.processWebhook(t.Context(), client, payload); err == nil {
				t.Error("history error was ignored")
			}
		})
	}
	wg.Wait()
	if got := peak.Load(); got != 1 || probes.Load() != 8 || mutations.Load() != 0 {
		t.Fatalf("history reads peak=%d probes=%d mutations=%d", got, probes.Load(), mutations.Load())
	}
}

// All media mutation tests use this local API and disposable disk fixtures.
// DELETE changes only the fake API's in-memory state, never even the fixture file.
type safetyFixture struct {
	mu                    sync.Mutex
	service               *Service
	client                *arr.Client
	file                  arr.MediaFile
	validation            probe.Validation
	deleted               bool
	queue                 []arr.QueueRecord
	episodes              []arr.Episode
	replacementEpisodes   []arr.Episode
	replacementFiles      []arr.MediaFile
	history               []arr.HistoryRecord
	mutations             []string
	commands              []arr.CommandRequest
	fail                  string
	before                func(*http.Request)
	autoRedownload        bool
	interactiveRedownload bool
	configResponse        any
	emptyQueueHistory     bool
}

func newSafetyFixture(t *testing.T, kind string) *safetyFixture {
	t.Helper()
	media := filepath.Join(t.TempDir(), "fixture.mkv")
	if err := os.WriteFile(media, []byte("fixture media"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(media)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(media))
	if err != nil {
		t.Fatal(err)
	}
	store, err := LoadStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &safetyFixture{file: arr.MediaFile{ID: 17, MovieID: 3, ParentID: 3, Path: media, RelativePath: "fixture.mkv", Size: info.Size(), Year: time.Now().Year()}, validation: probe.Validation{Reason: "no subtitles", FileInfo: info, DirectoryInfo: dir, SidecarsChecked: true}, queue: []arr.QueueRecord{}, episodes: []arr.Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 17, AirDate: "2025-01-01"}, {ID: 12, SeriesID: 3, EpisodeFileID: 17, AirDate: "2025-01-01"}}, history: []arr.HistoryRecord{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.before != nil {
			f.before(r)
		}
		key := r.Method + " " + r.URL.Path
		if r.Method != http.MethodGet {
			f.mutations = append(f.mutations, key)
		}
		if f.fail == key {
			http.Error(w, "injected failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		respond := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/system/status":
			respond(map[string]string{"appName": kind, "version": "fixture"})
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v3/moviefile/17" || r.URL.Path == "/api/v3/episodefile/17"):
			if f.deleted {
				w.WriteHeader(404)
				return
			}
			respond(f.file)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/3":
			respond(arr.Movie{ID: 3, Year: f.file.Year})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/series":
			respond([]arr.Series{{ID: 3, Year: f.file.Year}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			respond([]arr.Movie{{ID: 3, Year: f.file.Year}})
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v3/moviefile" || r.URL.Path == "/api/v3/episodefile"):
			if f.deleted {
				files := append([]arr.MediaFile{}, f.replacementFiles...)
				respond(files)
			} else {
				respond([]arr.MediaFile{f.file})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episode":
			episodes := append([]arr.Episode(nil), f.episodes...)
			if f.deleted {
				for i := range episodes {
					episodes[i].EpisodeFileID = 0
				}
				if f.replacementEpisodes != nil {
					episodes = append([]arr.Episode(nil), f.replacementEpisodes...)
				}
			}
			respond(episodes)
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v3/history/movie" || r.URL.Path == "/api/v3/history/series"):
			respond(f.history)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/history":
			history := f.history
			if len(history) == 0 && !f.emptyQueueHistory {
				for i, q := range f.queue {
					if strings.EqualFold(q.DownloadID, r.URL.Query().Get("downloadId")) {
						history = append(history, arr.HistoryRecord{ID: 1000 + i, DownloadID: q.DownloadID, SeriesID: q.SeriesID, MovieID: q.MovieID, EpisodeID: q.EpisodeID, EventType: "grabbed"})
					}
				}
			}
			respond(arr.HistoryPage{Records: history, TotalRecords: len(history)})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			respond(arr.QueuePage{Records: f.queue, TotalRecords: len(f.queue)})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/downloadclient":
			if f.configResponse != nil {
				respond(f.configResponse)
			} else {
				respond(map[string]bool{"autoRedownloadFailed": f.autoRedownload, "autoRedownloadFailedFromInteractiveSearch": f.interactiveRedownload})
			}
		case r.Method == http.MethodDelete && (r.URL.Path == "/api/v3/moviefile/17" || r.URL.Path == "/api/v3/episodefile/17"):
			f.deleted = true
			w.WriteHeader(204)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue/"):
			if r.URL.Query().Get("blocklist") != "true" || r.URL.Query().Get("skipRedownload") != "true" || r.URL.Query().Get("removeFromClient") != "true" {
				t.Error("unsafe queue flags")
			}
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/queue/"))
			origin := ""
			for _, q := range f.queue {
				if q.ID == id {
					origin = q.DownloadID
				}
			}
			if origin == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.queue = slices.DeleteFunc(f.queue, func(q arr.QueueRecord) bool { return strings.EqualFold(q.DownloadID, origin) })
			w.WriteHeader(204)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v3/history/failed/"):
			w.WriteHeader(204)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			var command arr.CommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Error(err)
			}
			f.commands = append(f.commands, command)
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected local request %s", key)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.client = arr.NewClient(config.Arr{Kind: kind, Name: kind, URL: server.URL, APIKey: "test", APIVersion: "v3"}, log)
	f.service = &Service{config: config.Config{MaxAttempts: 3, Workers: 2}, log: log, state: store, arr: map[string]*arr.Client{kind: f.client}, probeFn: func(context.Context, string) (probe.Validation, error) { return f.validation, nil }}
	return f
}

func (f *safetyFixture) apply(ctx context.Context) error {
	return f.service.applyValidation(ctx, f.client, f.file, f.validation, f.file.Path)
}

func TestInvalidMatchedFileWithoutHistoryIsSearchedWithoutBlocklist(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			f := newSafetyFixture(t, kind)
			if err := f.apply(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 2 || len(f.commands) != 1 || len(f.service.state.Pending()) != 0 {
				t.Fatalf("unexpected actions: %v", f.mutations)
			}
			if kind == "sonarr" && !slices.Equal(f.commands[0].EpisodeIDs, []int{10, 12}) {
				t.Fatal("did not use authoritative complete episode mapping")
			}
			if _, err := os.Stat(f.file.Path); err != nil {
				t.Fatal("fixture media was touched")
			}
		})
	}
}

func TestMutationFailuresRemainJournaledWithoutReplay(t *testing.T) {
	for _, phase := range []string{"DELETE /api/v3/moviefile/17", "DELETE /api/v3/queue/7", "POST /api/v3/command"} {
		t.Run(phase, func(t *testing.T) {
			f := newSafetyFixture(t, "radarr")
			f.history = []arr.HistoryRecord{{ID: 2, MovieID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}}
			f.queue = []arr.QueueRecord{{ID: 7, MovieID: 3, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}}
			f.fail = phase
			if err := f.apply(t.Context()); err == nil {
				t.Fatal("expected mutation error")
			}
			if len(f.service.state.Pending()) != 1 {
				t.Fatal("missing durable operation")
			}
			calls := len(f.mutations)
			if err := f.service.CleanupPending(t.Context()); err == nil {
				t.Fatal("missing reconciliation warning")
			}
			reloaded, err := LoadStateStore(f.service.state.path)
			if err != nil {
				t.Fatal(err)
			}
			f.service.state = reloaded
			if len(reloaded.Pending()) != 1 {
				t.Fatal("journal lost on restart")
			}
			f.fail = ""
			_ = f.apply(t.Context())
			if len(f.mutations) != calls {
				t.Fatal("uncertain action was replayed")
			}
			f.service.config.DryRun = true
			if err := f.service.CleanupPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != calls || len(reloaded.Pending()) != 1 {
				t.Fatal("dry-run cleanup changed state")
			}
		})
	}
}

func TestPreflightFailuresNeverDelete(t *testing.T) {
	for _, failure := range []string{"GET /api/v3/moviefile/17", "GET /api/v3/history/movie", "GET /api/v3/queue", "GET /api/v3/history", "GET /api/v3/config/downloadclient"} {
		t.Run(failure, func(t *testing.T) {
			f := newSafetyFixture(t, "radarr")
			f.history = []arr.HistoryRecord{{ID: 2, MovieID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}, {ID: 1, MovieID: 3, DownloadID: "origin", EventType: "grabbed"}}
			f.fail = failure
			if err := f.apply(t.Context()); err == nil {
				t.Fatal("expected preflight error")
			}
			if len(f.mutations) != 0 || len(f.service.state.Pending()) != 0 {
				t.Fatal("preflight failure caused mutation")
			}
		})
	}
}

func TestStaleMediaAndEpisodeMappingNeverDelete(t *testing.T) {
	for _, change := range []string{"path", "size", "parent", "mapping", "disk", "sidecar"} {
		t.Run(change, func(t *testing.T) {
			f := newSafetyFixture(t, "sonarr")
			original := f.file
			switch change {
			case "path":
				f.file.Path += ".replacement"
			case "size":
				f.file.Size++
			case "parent":
				f.file.ParentID++
			case "mapping":
				f.episodes = []arr.Episode{{ID: 10, SeriesID: 3, EpisodeFileID: 999}}
			case "disk":
				if err := os.WriteFile(f.file.Path, []byte("changed media fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "sidecar":
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.file.Path), "fixture.en.srt"), []byte("new subtitles"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.applyValidation(t.Context(), f.client, original, f.validation, original.Path); err == nil {
				t.Fatal("stale probe accepted")
			}
			if len(f.mutations) > 0 {
				t.Fatal("stale probe caused mutation")
			}
		})
	}
}

func TestValidProbeKeepsRetryCountsWhenSubtitleSnapshotIsStale(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, snapshot := range []string{"changed", "missing", "missingMedia"} {
			t.Run(kind+"/"+snapshot, func(t *testing.T) {
				f := newSafetyFixture(t, kind)
				keys := retryKeys(kind, f.file, []int{10, 12})
				for _, key := range keys {
					if _, err := f.service.state.Increment(key); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(f.service.state.path)
				if err != nil {
					t.Fatal(err)
				}
				f.validation.Valid = true
				if snapshot == "missing" {
					f.validation.DirectoryInfo = nil
				} else if snapshot == "missingMedia" {
					f.validation.FileInfo = nil
				} else if err := os.WriteFile(filepath.Join(filepath.Dir(f.file.Path), "fixture.en.srt"), []byte("new subtitles"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := f.apply(t.Context()); err == nil {
					t.Fatal("stale successful probe accepted")
				}
				after, err := os.ReadFile(f.service.state.path)
				if err != nil || string(before) != string(after) || len(f.mutations) != 0 {
					t.Fatal("stale successful probe changed state or Arr")
				}
			})
		}
	}
}

func TestConcurrentDuplicateWebhooksAndAuditOnlyDeleteOnce(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	payload := arr.WebhookPayload{EventType: "Download", EpisodeFile: &arr.WebhookFile{ID: 17}, Episodes: []arr.Episode{{ID: 999}}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { _ = f.service.processWebhook(t.Context(), f.client, payload) })
	}
	wg.Wait()
	if len(f.mutations) != 2 || len(f.commands) != 1 {
		t.Fatalf("duplicate remediation: %v", f.mutations)
	}
	if err := f.service.auditFile(t.Context(), f.client, f.file); err == nil {
		t.Fatal("stale scan unexpectedly succeeded")
	}
	if len(f.mutations) != 2 {
		t.Fatal("scan duplicated completed webhook remediation")
	}
}

func TestRetryLimitStillBlocklistsTerminalFile(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.history = []arr.HistoryRecord{{ID: 2, MovieID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}}
	f.queue = []arr.QueueRecord{{ID: 7, MovieID: 3, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}}
	key := retryKeys("radarr", f.file, nil)[0]
	for i := 0; i < 3; i++ {
		if _, err := f.service.state.Increment(key); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 2 || len(f.commands) != 0 {
		t.Fatal("terminal rejection was not blocklisted or was searched")
	}
}

func TestMultiEpisodeRetryKeysCarryAcrossReleaseGrouping(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	keys := retryKeys("sonarr", f.file, []int{10, 12})
	for i := 0; i < 3; i++ {
		if _, err := f.service.state.Increment(keys[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 0 || f.service.state.Attempts(keys[1]) != 4 {
		t.Fatal("episode retry cap bypassed by a pack")
	}
}

func TestStateWriteFailureRollsBackMemory(t *testing.T) {
	store, err := LoadStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Increment("key"); err != nil {
		t.Fatal(err)
	}
	store.path = t.TempDir() // rename onto a directory always fails
	if _, err := store.Increment("key"); err == nil {
		t.Fatal("expected write failure")
	}
	if store.Attempts("key") != 1 {
		t.Fatal("failed write mutated memory")
	}
	if err := store.Reset("key"); err == nil {
		t.Fatal("expected reset failure")
	}
	if store.Attempts("key") != 1 {
		t.Fatal("failed reset mutated memory")
	}
	if _, _, err := store.Begin("op", Operation{Kind: "radarr", SubjectID: 1, Phase: "delete"}, []string{"key"}); err == nil {
		t.Fatal("expected journal failure")
	}
	if len(store.Pending()) != 0 || store.Attempts("key") != 1 {
		t.Fatal("failed journal mutated memory")
	}
}

func TestStateLockAndServerBinding(t *testing.T) {
	cfg := config.Config{StatePath: filepath.Join(t.TempDir(), "state.json")}
	a, err := openServiceState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := openServiceState(cfg); err == nil {
		_ = b.Close()
		t.Fatal("second writer obtained lock")
	}
	if err := a.Bind(map[string]*arr.Client{"radarr": testArrClient("radarr", "http://first.invalid")}); err != nil {
		t.Fatal(err)
	}
	if err := a.Bind(map[string]*arr.Client{"radarr": testArrClient("radarr", "http://second.invalid")}); err == nil {
		t.Fatal("state reused across servers")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := openServiceState(cfg)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	_ = b.Close()
}

func TestCancellationBeforeMutationDoesNotChangeState(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated")
	}
	if len(f.mutations) != 0 || len(f.service.state.Pending()) > 0 {
		t.Fatal("canceled work mutated state")
	}
}

func TestQueueRecoveryRefusesSharedActiveAndUncertainMutations(t *testing.T) {
	for _, scenario := range []string{"activeSibling", "retryLimit", "uncertainDelete"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSafetyFixture(t, "sonarr")
			f.deleted = true
			f.queue = []arr.QueueRecord{{ID: 7, DownloadID: "pack", SeriesID: 3, EpisodeID: 10, Status: "completed", TrackedDownloadState: "importBlocked"}}
			switch scenario {
			case "activeSibling":
				q := f.queue[0]
				q.ID = 8
				q.EpisodeID = 12
				q.Status = "downloading"
				f.queue = append(f.queue, q)
			case "retryLimit":
				for i := 0; i < 3; i++ {
					if _, err := f.service.state.Increment("sonarr:episodes:10"); err != nil {
						t.Fatal(err)
					}
				}
			case "uncertainDelete":
				f.fail = "DELETE /api/v3/queue/7"
			}
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err == nil {
				t.Fatal("unsafe queue recovery accepted")
			}
			if scenario == "uncertainDelete" {
				f.fail = ""
				f.queue = []arr.QueueRecord{}
				_ = f.service.CleanupPending(t.Context())
				if len(f.mutations) != 1 || len(f.commands) != 0 || len(f.service.state.Pending()) != 1 {
					t.Fatal("queue disappearance treated as success")
				}
			} else if len(f.mutations) != 0 {
				t.Fatal("unsafe queue mutation")
			}
		})
	}
}

func TestStateMigratesLegacyEpisodeGroupsAndRejectsCorruption(t *testing.T) {
	for _, test := range []struct {
		data  string
		valid bool
	}{
		{`{"attempts":{"sonarr:episodes:10,12":3,"sonarr:episodes:10":1}}`, true},
		{`null`, false}, {`{"attempts":{"key":-1}}`, false}, {`{"operations":{"key":{"kind":"radarr","subjectId":0}}}`, false}, {`{"webhooks":{"bad":{"kind":"radarr","failures":-1}}}`, false}, {`{"attempts":{"sonarr:episodes:abc,12":3}}`, false},
	} {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := LoadStateStore(path)
		if !test.valid {
			if err == nil {
				t.Fatal("corrupt state accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if store.Attempts("sonarr:episodes:10") != 3 || store.Attempts("sonarr:episodes:12") != 3 || store.Attempts("sonarr:episodes:10,12") != 0 {
			t.Fatal("legacy retry migration lost cap")
		}
	}
}

func TestPathMappingPreservesUNCAndWindowsCase(t *testing.T) {
	s := &Service{config: config.Config{PathMappings: []config.PathMapping{{From: `C:\Media`, To: `\\server\share\media`}}}}
	if got := s.mapPath(`c:\media\Show\file.mkv`); got != `\\server\share\media\Show\file.mkv` {
		t.Fatalf("UNC/case mapping failed: %q", got)
	}
	s.config.PathMappings = []config.PathMapping{{From: "/media", To: "/mnt/media"}}
	if got := s.mapPath("/media/../other/file.mkv"); got != "/media/../other/file.mkv" {
		t.Fatal("traversal escaped mapping")
	}
}

func TestWebhookOriginMustBeConfirmedAndImportedPackIsSafeToFail(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	f.history = []arr.HistoryRecord{{ID: 2, SeriesID: 3, DownloadID: "origin", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}}
	f.queue = []arr.QueueRecord{{ID: 7, SeriesID: 3, EpisodeID: 10, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}, {ID: 8, SeriesID: 3, EpisodeID: 12, DownloadID: "origin", Status: "completed", TrackedDownloadState: "imported"}}
	payload := arr.WebhookPayload{EventType: "Download", EpisodeFile: &arr.WebhookFile{ID: 17}, DownloadID: "wrong"}
	if err := f.service.processWebhook(t.Context(), f.client, payload); err == nil || len(f.mutations) > 0 {
		t.Fatal("unconfirmed origin triggered mutation")
	}
	payload.DownloadID = "origin"
	if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 3 || len(f.commands) != 1 || !slices.Equal(f.commands[0].EpisodeIDs, []int{10, 12}) {
		t.Fatal("fully imported pack not handled safely")
	}
}
