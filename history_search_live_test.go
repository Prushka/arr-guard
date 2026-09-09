package main

import (
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Recheck previously subtitle-rejected files without ever invoking Arr mutations.
// IDs come from an operator's scan, e.g. sonarr:123,radarr:456. No paths or
// credentials are accepted here or printed in diagnostics.
func TestLiveRemediationSearchOwnership(t *testing.T) {
	raw := os.Getenv("ARR_LIVE_REMEDIATION_IDS")
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" || raw == "" {
		t.Skip("requires GET-only opt-in and explicit kind:file IDs")
	}
	values := strings.Split(raw, ",")
	if len(values) > 20 {
		t.Fatal("at most 20 diagnostic files are allowed")
	}
	cfg := loadLiveConfig(t)
	for _, value := range values {
		kind, idText, ok := strings.Cut(strings.TrimSpace(value), ":")
		id, err := strconv.Atoi(idText)
		if !ok || id < 1 || err != nil || (kind != "sonarr" && kind != "radarr") {
			t.Fatal("expected sonarr:ID or radarr:ID")
		}
		t.Run(kind+"/"+idText, func(t *testing.T) {
			ac := cfg.Sonarr
			if kind == "radarr" {
				ac = cfg.Radarr
			}
			if ac == nil {
				t.Fatal("requested Arr instance is not configured")
			}
			base, err := url.Parse(ac.URL)
			if err != nil {
				t.Fatal("invalid configured URL")
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			rt := &liveReadTransport{base: base}
			client := NewArrClient(*ac, log)
			client.readOnly = true
			client.client.Transport = rt
			service := &Service{config: cfg, log: log, state: &StateStore{state: State{Attempts: map[string]int{}}}, arr: map[string]*ArrClient{kind: client}, probe: Prober{Path: cfg.FFprobePath, Timeout: 2 * time.Minute}}
			file, err := client.GetMediaFile(t.Context(), id)
			if err != nil {
				t.Fatalf("file read: %s", liveErrorCategory(err))
			}
			if kind == "sonarr" {
				file.Year, err = client.EpisodeReleaseYearForFile(t.Context(), file.ParentID, file.ID)
			} else {
				var movie Movie
				err = client.do(t.Context(), "GET", client.apiPath("movie", strconv.Itoa(file.MovieID)), nil, nil, &movie)
				if err == nil && movie.ID != file.MovieID {
					t.Fatal("movie identity mismatch")
				}
				file.Year = movie.Year
			}
			if err != nil {
				t.Fatalf("metadata read: %s", liveErrorCategory(err))
			}
			v, path, err := service.validate(t.Context(), file)
			if err != nil {
				t.Fatalf("probe: %s", liveErrorCategory(err))
			}
			if v.Valid || shouldSkipSilentMedia(file.Year, time.Now()) {
				t.Fatal("candidate now passes or is age-excluded; choose a current policy rejection")
			}
			downloadID, _, err := service.findOrigin(t.Context(), client, file)
			if err != nil {
				t.Fatalf("history read: %s", liveErrorCategory(err))
			}
			origin, err := service.prepareOrigin(t.Context(), client, file, downloadID)
			if err != nil {
				t.Fatalf("origin preflight: %s", liveErrorCategory(err))
			}
			if origin.historyID < 1 {
				t.Fatal("candidate no longer needs history failure; choose a current history candidate")
			}
			if err := service.applyValidation(t.Context(), client, file, v, path); err != nil {
				t.Fatalf("remediation dry run: %s", liveErrorCategory(err))
			}
			payload := WebhookPayload{EventType: "Download", DownloadID: downloadID}
			if kind == "sonarr" {
				payload.EpisodeFile = &WebhookFile{ID: id}
				payload.Series = &Series{ID: file.ParentID}
			} else {
				payload.EventType = "ImportComplete"
				payload.MovieFile = &WebhookFile{ID: id}
				payload.Movie = &Movie{ID: file.MovieID}
			}
			if err := service.processWebhook(t.Context(), client, payload); err != nil {
				t.Fatalf("webhook dry run: %s", liveErrorCategory(err))
			}
			state := service.state.state
			if len(state.Attempts)+len(state.Operations)+len(state.Completed)+len(state.Webhooks) != 0 {
				t.Fatal("dry run changed retry state")
			}
			t.Logf("policy_rejected=true history_preflight=passed arr_automatic_search=%t webhook=%s GET_requests=%d mutations=0 retry_writes=0", origin.automaticSearch, payload.EventType, rt.reads.Load())
		})
	}
}
