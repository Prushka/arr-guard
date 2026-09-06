package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebhookPayloadFiles(t *testing.T) {
	sonarr := WebhookPayload{EpisodeFile: &WebhookFile{ID: 1}}
	if got := sonarr.files("sonarr"); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("unexpected Sonarr files: %#v", got)
	}
	radarr := WebhookPayload{MovieFiles: []WebhookFile{{ID: 2}, {ID: 3}}}
	if got := radarr.files("radarr"); len(got) != 2 || got[1].ID != 3 {
		t.Fatalf("unexpected Radarr files: %#v", got)
	}
}

func TestOldUnknownSubtitleGrace(t *testing.T) {
	now := time.Now()
	validation := Validation{HasSubtitles: true, HasUnknownLanguage: true, Reason: "no English subtitle stream or sidecar"}
	accepted := applyOldMediaGrace(validation, now.Year()-11, now)
	if !accepted.Valid || accepted.Reason == validation.Reason {
		t.Fatalf("old unknown subtitle was not accepted: %#v", accepted)
	}
	if recent := applyOldMediaGrace(validation, now.Year()-10, now); recent.Valid {
		t.Fatalf("media exactly ten years old unexpectedly accepted: %#v", recent)
	}
	if noSubtitles := applyOldMediaGrace(Validation{HasUnknownLanguage: true}, now.Year()-11, now); noSubtitles.Valid {
		t.Fatalf("media without subtitles unexpectedly accepted: %#v", noSubtitles)
	}
}

func TestSilentMediaSkipBoundary(t *testing.T) {
	now := time.Now()
	if !shouldSkipSilentMedia(now.Year()-51, now) {
		t.Fatal("media older than 50 years was not skipped")
	}
	if shouldSkipSilentMedia(now.Year()-50, now) {
		t.Fatal("media exactly 50 years old was unexpectedly skipped")
	}
	if shouldSkipSilentMedia(0, now) {
		t.Fatal("media with an unknown year was unexpectedly skipped")
	}
}

func TestAuditSkipsSilentMediaBeforeProbe(t *testing.T) {
	probed := false
	service := &Service{
		log: slog.Default(),
		probeFn: func(context.Context, string) (Validation, error) {
			probed = true
			return Validation{}, nil
		},
	}
	file := MediaFile{ID: 7, MovieID: 3, Year: time.Now().Year() - 51, Path: "Movie.mkv"}
	if err := service.auditFile(context.Background(), testArrClient("radarr", "http://arr.invalid"), file); err != nil {
		t.Fatal(err)
	}
	if probed {
		t.Fatal("subtitle guard probed media older than 50 years")
	}
}

func TestWebhookSkipsSilentMovieBeforeProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/3" {
			_ = json.NewEncoder(w).Encode(Movie{ID: 3, Year: time.Now().Year() - 51})
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/moviefile/7" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"movieId":3,"path":"/media/Movie.mkv"}`))
	}))
	defer server.Close()

	probed := false
	service := &Service{
		log: slog.Default(),
		probeFn: func(context.Context, string) (Validation, error) {
			probed = true
			return Validation{}, nil
		},
	}
	payload := WebhookPayload{
		EventType:  "Download",
		DownloadID: "download-id",
		Movie:      &Movie{ID: 3, Year: time.Now().Year() - 51},
		MovieFile:  &WebhookFile{ID: 7},
	}
	if err := service.processWebhook(context.Background(), testArrClient("radarr", server.URL), payload); err != nil {
		t.Fatal(err)
	}
	if probed {
		t.Fatal("webhook subtitle guard probed movie older than 50 years")
	}
}

func TestWebhookUsesEpisodeReleaseYearForSilentMediaSkip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/episodefile/7":
			if r.Method != http.MethodGet {
				t.Fatalf("episode file method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"id":7,"seriesId":3,"path":"/media/Show/S01E01.mkv"}`))
		case "/api/v3/episode":
			if r.Method != http.MethodGet || r.URL.Query().Get("seriesId") != "3" {
				t.Fatalf("episode request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id":8,"seriesId":3,"episodeFileId":7,"airDate":"1920-01-01"}]`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	probed := false
	service := &Service{
		log: slog.Default(),
		probeFn: func(context.Context, string) (Validation, error) {
			probed = true
			return Validation{}, nil
		},
	}
	payload := WebhookPayload{
		EventType:   "Download",
		DownloadID:  "download-id",
		Series:      &Series{ID: 3, Year: 1920},
		EpisodeFile: &WebhookFile{ID: 7},
	}
	if err := service.processWebhook(context.Background(), testArrClient("sonarr", server.URL), payload); err != nil {
		t.Fatal(err)
	}
	if probed {
		t.Fatal("webhook subtitle guard probed episode older than 50 years")
	}
}

func TestWebhookDoesNotSkipModernEpisodeOfOldSeries(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	f.service.config.DryRun = true
	f.history = []HistoryRecord{{ID: 1, SeriesID: 3, DownloadID: "download-id", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "17"}}}
	f.validation.Valid = true
	probed := false
	f.service.probeFn = func(context.Context, string) (Validation, error) {
		probed = true
		return f.validation, nil
	}
	payload := WebhookPayload{
		EventType:   "Download",
		DownloadID:  "download-id",
		Series:      &Series{ID: 3, Year: 1920},
		EpisodeFile: &WebhookFile{ID: 17},
	}
	if err := f.service.processWebhook(t.Context(), f.client, payload); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Fatal("webhook subtitle guard skipped a modern episode of an old series")
	}
}

func TestDryRunDoesNotMutateRetryStateOrCallArrMutations(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		f := newSafetyFixture(t, kind)
		f.service.config.DryRun = true
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
		for _, valid := range []bool{false, true} {
			f.validation.Valid = valid
			if err := f.apply(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		after, err := os.ReadFile(f.service.state.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) || len(f.mutations) != 0 {
			t.Fatal("dry-run mutated state or Arr")
		}
	}
}

func TestHistoryDataIsCaseInsensitive(t *testing.T) {
	record := HistoryRecord{Data: map[string]string{"FileId": "17"}}
	if got := historyData(record, "fileId"); got != "17" {
		t.Fatalf("history data = %q", got)
	}
}

func TestRetryKeyUsesStableSonarrEpisodeIDs(t *testing.T) {
	file := MediaFile{ParentID: 3, RelativePath: "Show.mkv"}
	got := retryKey("sonarr", file, []int{12, 10}, file.RelativePath)
	if got != "sonarr:episodes:10,12" {
		t.Fatalf("retry key = %q", got)
	}
}

func TestRetryKeyUsesStableRadarrMovieID(t *testing.T) {
	file := MediaFile{MovieID: 3, RelativePath: "Movie.mkv"}
	if got := retryKey("radarr", file, nil, file.RelativePath); got != "radarr:movie:3" {
		t.Fatalf("retry key = %q", got)
	}
}

func TestValidSonarrWebhookStyleResetClearsEpisodeAndLegacyPathKeys(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	keys := retryKeys("sonarr", f.file, []int{10, 12})
	keys = append(keys, retryKey("sonarr", f.file, []int{10, 12}, f.file.RelativePath), retryKey("sonarr", f.file, nil, f.file.RelativePath))
	for _, key := range keys {
		if _, err := f.service.state.Increment(key); err != nil {
			t.Fatal(err)
		}
	}
	f.validation.Valid = true
	if err := f.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if got := f.service.state.Attempts(key); got != 0 {
			t.Fatalf("retry attempts for %s = %d, want 0", key, got)
		}
	}
}

func TestWebhookStateExpiresAndLockEntriesAreReleased(t *testing.T) {
	service := &Service{}
	if !service.claimWebhook("sonarr:7") || service.claimWebhook("sonarr:7") {
		t.Fatal("webhook claim did not deduplicate")
	}
	service.handledMu.Lock()
	service.handled["sonarr:7"] = time.Now().Add(-webhookDedupTTL - time.Second)
	service.handledMu.Unlock()
	service.cleanupWebhookState()
	if service.claimWebhook("sonarr:7") == false {
		t.Fatal("expired webhook claim was not released")
	}
	lock := service.acquireFileLock("sonarr:7")
	service.releaseFileLock("sonarr:7", lock)
	service.locksMu.Lock()
	defer service.locksMu.Unlock()
	if len(service.locks) != 0 {
		t.Fatalf("released lock remains: %#v", service.locks)
	}
}

func TestWebhookHandlerAuthAndEnqueue(t *testing.T) {
	client := testArrClient("sonarr", "http://arr.invalid")
	service := &Service{
		config: Config{WebhookToken: "token"},
		arr:    map[string]*ArrClient{"sonarr": client},
		jobs:   make(chan webhookJob, 1),
	}
	payload, err := json.Marshal(WebhookPayload{EventType: "Download", EpisodeFile: &WebhookFile{ID: 7}})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/webhook/sonarr", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	service.WebhookHandler("sonarr")(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/webhook/sonarr", bytes.NewReader(payload))
	request.Header.Set("X-Webhook-Token", "token")
	response = httptest.NewRecorder()
	service.WebhookHandler("sonarr")(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("authorized status = %d", response.Code)
	}
	job := <-service.jobs
	if job.client != client || job.payload.EventType != "Download" || job.payload.EpisodeFile == nil || job.payload.EpisodeFile.ID != 7 {
		t.Fatalf("enqueued job = %#v", job)
	}
}

func TestWebhookHandlerBasicAuth(t *testing.T) {
	client := testArrClient("sonarr", "http://arr.invalid")
	service := &Service{
		config: Config{WebhookUsername: "sonarr", WebhookPassword: "secret"},
		arr:    map[string]*ArrClient{"sonarr": client},
		jobs:   make(chan webhookJob, 1),
	}
	payload := []byte(`{"eventType":"Download","episodeFile":{"id":7}}`)

	request := httptest.NewRequest(http.MethodPost, "/webhook/sonarr", bytes.NewReader(payload))
	request.SetBasicAuth("sonarr", "wrong")
	response := httptest.NewRecorder()
	service.WebhookHandler("sonarr")(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong basic auth status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/webhook/sonarr", bytes.NewReader(payload))
	request.SetBasicAuth("sonarr", "secret")
	response = httptest.NewRecorder()
	service.WebhookHandler("sonarr")(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("valid basic auth status = %d", response.Code)
	}
}

func TestRecoverBlockedQueueGroupsDownloadAndLimitsSearches(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			f := newSafetyFixture(t, kind)
			f.deleted = true // blocked imports have no managed file yet
			f.queue = []QueueRecord{{ID: 7, DownloadID: "blocked", MovieID: 3, SeriesID: 3, EpisodeID: 10, Status: "completed", TrackedDownloadState: "importBlocked"}}
			if kind == "sonarr" {
				second := f.queue[0]
				second.ID = 8
				second.EpisodeID = 12
				f.queue = append(f.queue, second)
			}
			if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 2 || len(f.commands) != 1 {
				t.Fatalf("actions = %v", f.mutations)
			}
			if kind == "sonarr" && !equalInts(f.commands[0].EpisodeIDs, []int{10, 12}) {
				t.Fatal("shared download was not fully searched")
			}
		})
	}
}

func TestRecoverBlockedQueueDryRunDoesNotMutate(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.service.config.DryRun = true
	f.deleted = true
	f.queue = []QueueRecord{{ID: 7, DownloadID: "blocked", MovieID: 3, Status: "completed", TrackedDownloadState: "importBlocked"}}
	if err := f.service.recoverBlockedQueue(t.Context(), f.client); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) > 0 || len(f.service.state.Pending()) > 0 {
		t.Fatal("queue dry run mutated state")
	}
}

func TestQueueRecordNeedsImportRecovery(t *testing.T) {
	tests := []struct {
		name string
		item QueueRecord
		want bool
	}{
		{name: "blocked state", item: QueueRecord{Status: "completed", TrackedDownloadState: "importBlocked"}, want: true},
		{name: "blocked message", item: QueueRecord{Status: "completed", StatusMessages: []QueueStatusMessage{{Messages: []string{"Unable to Import Automatically"}}}}, want: true},
		{name: "still downloading", item: QueueRecord{Status: "downloading", TrackedDownloadState: "importBlocked"}, want: false},
		{name: "ordinary completed", item: QueueRecord{Status: "completed", TrackedDownloadState: "importPending"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.item.needsImportRecovery(); got != test.want {
				t.Fatalf("needsImportRecovery() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestLoadConfigRequiresBasicAuthPair(t *testing.T) {
	t.Setenv("SONARR_URL", "http://sonarr.invalid")
	t.Setenv("SONARR_API_KEY", "key")
	t.Setenv("RADARR_URL", "")
	t.Setenv("RADARR_API_KEY", "")
	t.Setenv("WEBHOOK_TOKEN", "")
	t.Setenv("WEBHOOK_USERNAME", "guard")
	t.Setenv("WEBHOOK_PASSWORD", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig accepted only one Basic Auth credential")
	}
}

func TestMapPath(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "state.json")
	store, err := LoadStateStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		config: Config{PathMappings: []PathMapping{{From: "/tv", To: "/media/tv"}}},
		log:    slog.Default(),
		state:  store,
	}
	if got := service.mapPath("/tv/Show/S01E01.mkv"); got != "/media/tv/Show/S01E01.mkv" {
		t.Fatalf("mapped path = %q", got)
	}
	service.config.PathMappings = []PathMapping{{From: `C:\Media\TV`, To: `D:\Mounted\TV`}}
	if got := service.mapPath(`C:\Media\TV\Show\episode.mkv`); got != `D:\Mounted\TV\Show\episode.mkv` {
		t.Fatalf("Windows mapped path = %q", got)
	}
	service.config.PathMappings = []PathMapping{{From: "/tv", To: "/media/tv"}}
	if got := service.mapPath("/movies/film.mkv"); got != "/movies/film.mkv" {
		t.Fatalf("unmapped path changed to %q", got)
	}
	if got := service.mapPath("/tv2/Show/S01E01.mkv"); got != "/tv2/Show/S01E01.mkv" {
		t.Fatalf("path with similar prefix was mapped to %q", got)
	}

	service.config.PathMappings = []PathMapping{
		{From: "/media", To: "/mounted"},
		{From: "/media/tv", To: "/mounted/tv"},
	}
	if got := service.mapPath("/media/tv/Show/episode.mkv"); got != "/mounted/tv/Show/episode.mkv" {
		t.Fatalf("most-specific path mapping = %q", got)
	}
}

func TestScanRootsDeduplicatesNestedDestinations(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "media")
	got := scanRoots([]PathMapping{
		{From: "/media", To: parent},
		{From: "/movies", To: filepath.Join(parent, "movies")},
		{From: "/tv", To: filepath.Join(t.TempDir(), "tv")},
	})
	if len(got) != 2 {
		t.Fatalf("scan roots = %#v, want two non-overlapping roots", got)
	}
	foundParent := false
	for _, root := range got {
		if root == filepath.Clean(parent) {
			foundParent = true
		}
		if root == filepath.Clean(filepath.Join(parent, "movies")) {
			t.Fatalf("nested scan root was not deduplicated: %#v", got)
		}
	}
	if !foundParent {
		t.Fatalf("parent scan root missing: %#v", got)
	}
}

func TestIsMediaPath(t *testing.T) {
	for _, value := range []string{"Movie.MKV", "movie.mp4", "episode.sup"} {
		if value == "episode.sup" {
			if isMediaPath(value) {
				t.Fatalf("subtitle path %q classified as media", value)
			}
			continue
		}
		if !isMediaPath(value) {
			t.Fatalf("media path %q was not recognized", value)
		}
	}
}

func TestWriteUnmatchedReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "unmatched.json")
	report := UnmatchedReport{
		GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Roots:       []string{"D:\\Media"},
		Files:       []UnmatchedMedia{{Path: "D:\\Media\\orphan.mkv"}},
	}
	if err := writeUnmatchedReport(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"validation"`) {
		t.Fatalf("unmatched report unexpectedly contains validation: %s", data)
	}
	var decoded UnmatchedReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Files) != 1 || decoded.Files[0].Path != report.Files[0].Path {
		t.Fatalf("decoded report = %#v", decoded)
	}
}

func TestScanUnmatchedWritesAllOrphansWithoutProbing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(filepath.Join(root, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Orphans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Excluded"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"matched":  filepath.Join(root, "Show", "matched.mkv"),
		"invalid":  filepath.Join(root, "Orphans", "invalid.mp4"),
		"valid":    filepath.Join(root, "Orphans", "valid.webm"),
		"sidecar":  filepath.Join(root, "Orphans", "subtitle.sup"),
		"excluded": filepath.Join(root, "Excluded", "hidden.mkv"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unmatched scan made mutating request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/series":
			_, _ = w.Write([]byte(`[{"id":1,"year":2020,"path":"/media/Show"}]`))
		case "/api/v3/episodefile":
			if r.URL.Query().Get("seriesId") != "1" {
				t.Errorf("episodefile query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id":10,"seriesId":1,"relativePath":"matched.mkv","path":"/media/Show/matched.mkv"}]`))
		default:
			t.Errorf("unexpected unmatched scan request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	reportPath := filepath.Join(t.TempDir(), "unmatched.json")
	service := &Service{
		config: Config{
			PathMappings:         []PathMapping{{From: "/media", To: root}},
			UnmatchedPath:        reportPath,
			Workers:              2,
			UnmatchedExcludeDirs: []string{filepath.Join(root, "Excluded")},
		},
		log: slog.Default(),
		arr: map[string]*ArrClient{"sonarr": testArrClient("sonarr", server.URL)},
	}
	if err := service.ScanUnmatched(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report UnmatchedReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 2 || report.Files[0].Path != paths["invalid"] || report.Files[1].Path != paths["valid"] {
		t.Fatalf("unmatched orphan report = %#v", report)
	}
	for _, file := range report.Files {
		if file.Path == paths["excluded"] || file.Path == paths["matched"] {
			t.Fatalf("unexpected path in orphan report: %#v", file)
		}
	}
}

func TestStateStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := LoadStateStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, err := store.Increment("radarr:1:film.mkv"); err != nil || attempt != 1 {
		t.Fatalf("first increment = %d, %v", attempt, err)
	}
	if attempt, err := store.Increment("radarr:1:film.mkv"); err != nil || attempt != 2 {
		t.Fatalf("second increment = %d, %v", attempt, err)
	}
	if err := store.Reset("radarr:1:film.mkv"); err != nil {
		t.Fatal(err)
	}
	if got := store.Attempts("radarr:1:film.mkv"); got != 0 {
		t.Fatalf("attempts after reset = %d", got)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
}
