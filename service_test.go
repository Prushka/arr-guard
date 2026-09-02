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

func TestEpisodeIDs(t *testing.T) {
	payload := WebhookPayload{Episodes: []Episode{{ID: 10}, {}, {ID: 12}}}
	if got := episodeIDs(payload); len(got) != 2 || got[0] != 10 || got[1] != 12 {
		t.Fatalf("episode IDs = %#v", got)
	}
}

func TestEpisodeIDsForFileUsesFileMapping(t *testing.T) {
	payload := WebhookPayload{
		EpisodeFiles: []WebhookFile{{ID: 7}, {ID: 8}},
		Episodes: []Episode{
			{ID: 10, EpisodeFileID: 7},
			{ID: 11, EpisodeFileID: 8},
			{ID: 12, EpisodeFileID: 7},
		},
	}
	if got := episodeIDsForFile(payload, 7); !equalInts(got, []int{10, 12}) {
		t.Fatalf("episode IDs for file = %#v", got)
	}
}

func TestEpisodeIDsForFileSingleFileFallback(t *testing.T) {
	payload := WebhookPayload{
		EpisodeFile: &WebhookFile{ID: 7},
		Episodes:    []Episode{{ID: 10}, {ID: 12}},
	}
	if got := episodeIDsForFile(payload, 7); !equalInts(got, []int{10, 12}) {
		t.Fatalf("episode IDs for single file = %#v", got)
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

func TestDryRunDoesNotMutateRetryStateOrCallArr(t *testing.T) {
	file := MediaFile{ID: 7, MovieID: 3, RelativePath: "Movie.mkv", Path: "Movie.mkv"}
	key := retryKey("radarr", file, nil, file.RelativePath)
	store := &StateStore{state: State{Attempts: map[string]int{key: 2}}}
	service := &Service{
		config: Config{DryRun: true, MaxAttempts: 3},
		log:    slog.Default(),
		state:  store,
	}
	client := testArrClient("radarr", "http://arr.invalid")

	if err := service.applyValidation(context.Background(), client, file, Validation{Valid: true}, file.Path, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.Attempts(key); got != 2 {
		t.Fatalf("valid dry-run changed retry attempts to %d", got)
	}

	invalid := Validation{Reason: "no subtitles"}
	if err := service.applyValidation(context.Background(), client, file, invalid, file.Path, "download-id", 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.Attempts(key); got != 2 {
		t.Fatalf("invalid dry-run changed retry attempts to %d", got)
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/episode" || r.URL.Query().Get("seriesId") != "42" {
			t.Fatalf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":314,"episodeFileId":7}]`))
	}))
	defer server.Close()

	store, err := LoadStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	file := MediaFile{ID: 7, ParentID: 42, RelativePath: "Show - S01E01.mkv", Path: "Show - S01E01.mkv"}
	episodeKey := retryKey("sonarr", file, []int{314}, file.RelativePath)
	legacyKey := retryKey("sonarr", file, nil, file.RelativePath)
	if _, err := store.Increment(episodeKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Increment(legacyKey); err != nil {
		t.Fatal(err)
	}

	service := &Service{config: Config{DryRun: false}, log: slog.Default(), state: store}
	if err := service.applyValidationOptions(context.Background(), testArrClient("sonarr", server.URL), file, Validation{Valid: true}, file.Path, "", 0, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := store.Attempts(episodeKey); got != 0 {
		t.Fatalf("episode retry attempts = %d, want 0", got)
	}
	if got := store.Attempts(legacyKey); got != 0 {
		t.Fatalf("legacy retry attempts = %d, want 0", got)
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

func TestRecoverBlockedQueueRadarrRemovesBlocklistsAndSearches(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			requests = append(requests, "queue")
			_, _ = w.Write([]byte(`{"records":[
                {"id":1,"downloadId":"active","movieId":12,"status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading"},
                {"id":23,"downloadId":"blocked","movieId":91,"status":"completed","trackedDownloadStatus":"warning","trackedDownloadState":"importBlocked","statusMessages":[{"title":"Unable to Import Automatically","messages":["Manual import required"]}]}
            ]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/23":
			requests = append(requests, "fail")
			if r.URL.Query().Get("removeFromClient") != "true" || r.URL.Query().Get("blocklist") != "true" || r.URL.Query().Get("skipRedownload") != "true" {
				t.Fatalf("queue recovery query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			requests = append(requests, "search")
			var command CommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Fatal(err)
			}
			if command.Name != "MoviesSearch" || !equalInts(command.MovieIDs, []int{91}) {
				t.Fatalf("movie search command = %#v", command)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := &Service{
		config: Config{StatePath: filepath.Join(t.TempDir(), "state.json")},
		log:    slog.Default(),
	}
	store, err := LoadStateStore(service.config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service.state = store
	client := testArrClient("radarr", server.URL)
	if err := service.recoverBlockedQueue(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(requests, ","), "queue,fail,search"; got != want {
		t.Fatalf("request order = %q, want %q", got, want)
	}
}

func TestRecoverBlockedQueueSonarrSearchesOnlyEpisode(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			requests = append(requests, "queue")
			_, _ = w.Write([]byte(`{"records":[{"id":7,"downloadId":"blocked","seriesId":42,"episodeId":314,"status":"completed","trackedDownloadState":"importBlocked"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/7":
			requests = append(requests, "fail")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			requests = append(requests, "search")
			var command CommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Fatal(err)
			}
			if command.Name != "EpisodeSearch" || !equalInts(command.EpisodeIDs, []int{314}) || command.SeriesID != 0 {
				t.Fatalf("episode search command = %#v", command)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := &Service{config: Config{StatePath: filepath.Join(t.TempDir(), "state.json")}, log: slog.Default()}
	store, err := LoadStateStore(service.config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service.state = store
	if err := service.recoverBlockedQueue(context.Background(), testArrClient("sonarr", server.URL)); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(requests, ","), "queue,fail,search"; got != want {
		t.Fatalf("request order = %q, want %q", got, want)
	}
}

func TestRecoverBlockedQueueDryRunDoesNotMutate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/queue" {
			t.Fatalf("dry-run made mutating request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"records":[{"id":7,"movieId":91,"status":"completed","trackedDownloadState":"importBlocked"}]}`))
	}))
	defer server.Close()

	service := &Service{config: Config{DryRun: true, StatePath: filepath.Join(t.TempDir(), "state.json")}, log: slog.Default()}
	store, err := LoadStateStore(service.config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service.state = store
	if err := service.recoverBlockedQueue(context.Background(), testArrClient("radarr", server.URL)); err != nil {
		t.Fatal(err)
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
			InvalidPath:          reportPath,
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

func TestInvalidMatchedFileWithoutHistoryIsSearchedWithoutBlocklist(t *testing.T) {
	requests := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/moviefile/17":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			var command CommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Fatal(err)
			}
			if command.Name != "MoviesSearch" || !equalInts(command.MovieIDs, []int{3}) {
				t.Fatalf("command = %#v", command)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	store, err := LoadStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		config: Config{DryRun: false, MaxAttempts: 3},
		log:    slog.Default(),
		state:  store,
	}
	file := MediaFile{ID: 17, MovieID: 3, RelativePath: "Movie.mkv", Path: "Movie.mkv"}
	if err := service.applyValidation(context.Background(), testArrClient("radarr", server.URL), file, Validation{Reason: "no subtitles"}, file.Path, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(requests); got != 2 {
		t.Fatalf("request count = %d, want delete and search only", got)
	}
	for i := 0; i < 2; i++ {
		if got := <-requests; got == "GET /api/v3/queue" || got == "POST /api/v3/history/failed/" {
			t.Fatalf("unexpected blocklist request %q", got)
		}
	}
}

func TestPendingRemediationCleanupRetriesPostDeleteWork(t *testing.T) {
	var queueCalls, searchCalls, queueDeleteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/moviefile/17":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			queueCalls++
			if queueCalls == 1 {
				http.Error(w, "temporary queue failure", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"records":[{"id":77,"downloadId":"download-1","movieId":3,"status":"completed"}],"totalRecords":1}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/77":
			queueDeleteCalls++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			searchCalls++
			if searchCalls == 1 {
				http.Error(w, "temporary search failure", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store, err := LoadStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		config: Config{DryRun: false, MaxAttempts: 3},
		log:    slog.Default(),
		state:  store,
	}
	file := MediaFile{ID: 17, MovieID: 3, RelativePath: "Movie.mkv", Path: "Movie.mkv"}
	client := testArrClient("radarr", server.URL)
	if err := service.applyValidation(context.Background(), client, file, Validation{Reason: "no subtitles"}, file.Path, "download-1", 0, nil); err == nil {
		t.Fatal("applyValidation unexpectedly succeeded despite post-delete API failures")
	}
	service.pendingMu.Lock()
	pendingCount := len(service.pending)
	service.pendingMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending remediations = %d, want 1", pendingCount)
	}

	if err := service.CleanupPending(context.Background()); err != nil {
		t.Fatalf("CleanupPending() error = %v", err)
	}
	if queueCalls != 2 || queueDeleteCalls != 1 || searchCalls != 2 {
		t.Fatalf("cleanup calls: queue=%d queueDelete=%d search=%d, want 2, 1, 2", queueCalls, queueDeleteCalls, searchCalls)
	}
	service.pendingMu.Lock()
	defer service.pendingMu.Unlock()
	if len(service.pending) != 0 {
		t.Fatalf("pending remediations remain after successful cleanup: %#v", service.pending)
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
