package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
