package guard

import (
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/probe"
)

// Exercise actual probes and both scan/webhook preflights, never Arr mutations.
// Logs contain file IDs and fixed categories, not server/media paths or stderr.
func TestLiveProbeDiagnostics(t *testing.T) {
	raw := os.Getenv("ARR_LIVE_PROBE_IDS")
	expectedPath := os.Getenv("ARR_LIVE_PROBE_PATH")
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" || (raw == "" && expectedPath == "") {
		t.Skip("requires GET-only opt-in and explicit kind:file IDs")
	}
	var values []string
	if raw != "" {
		values = strings.Split(raw, ",")
	}
	exampleFound := false
	cfg := loadLiveConfig(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clients := map[string]*arr.Client{}
	transports := map[string]*liveReadTransport{}
	for _, ac := range []*config.Arr{cfg.Sonarr, cfg.Radarr} {
		if ac == nil {
			continue
		}
		base, err := url.Parse(ac.URL)
		if err != nil {
			t.Fatal("invalid configured URL")
		}
		client := arr.NewClient(*ac, log)
		client.EnforceReadOnly()
		rt := &liveReadTransport{base: base}
		client.SetTransport(rt)
		clients[ac.Kind], transports[ac.Kind] = client, rt
	}
	if expectedPath != "" {
		id := liveSonarrProbeFileID(t, clients["sonarr"], expectedPath)
		value := "sonarr:" + strconv.Itoa(id)
		if !slices.Contains(values, value) {
			values = append(values, value)
		}
		t.Logf("reported_example_file_id=%d", id)
	}
	if len(values) > 20 {
		t.Fatal("at most 20 diagnostic files are allowed")
	}
	service := &Service{config: cfg, log: log, arr: clients, state: &StateStore{state: State{Attempts: map[string]int{}}}, probe: probe.Prober{Path: cfg.FFprobePath, Timeout: 2 * time.Minute}}
	accepted, rejected, blocked, recovered, preflightBlocked := 0, 0, 0, 0, 0
	for _, value := range values {
		kind, idText, ok := strings.Cut(strings.TrimSpace(value), ":")
		id, err := strconv.Atoi(idText)
		if !ok || err != nil || id < 1 || clients[kind] == nil {
			t.Fatal("expected a configured sonarr:ID or radarr:ID")
		}
		client := clients[kind]
		t.Run(kind+"/"+idText, func(t *testing.T) {
			file, err := client.GetMediaFile(t.Context(), id)
			if err != nil {
				t.Fatalf("file read: %s", liveErrorCategory(err))
			}
			if expectedPath != "" && file.Path == expectedPath {
				exampleFound = true
			}
			if kind == "sonarr" {
				file.Year, err = client.EpisodeReleaseYearForFile(t.Context(), file.ParentID, file.ID)
			} else {
				var movie arr.Movie
				movie, err = client.GetMovie(t.Context(), file.MovieID)
				if err == nil && movie.ID != file.MovieID {
					t.Fatal("movie identity mismatch")
				}
				file.Year = movie.Year
			}
			if err != nil {
				t.Fatalf("metadata read: %s", liveErrorCategory(err))
			}
			v, path, probeErr := service.validate(t.Context(), file)
			repeated, _, repeatErr := service.validate(t.Context(), file)
			if probeErr != nil {
				blocked++
				if repeatErr == nil || liveErrorCategory(probeErr) != liveErrorCategory(repeatErr) {
					t.Error("repeated probe did not reproduce the failure category")
				}
				t.Logf("probe_blocked=true category=%s", liveErrorCategory(probeErr))
				return
			}
			if repeatErr != nil || v.Valid != repeated.Valid || !slices.Equal(v.SubtitleLangs, repeated.SubtitleLangs) || !slices.Equal(v.ProbeWarnings, repeated.ProbeWarnings) {
				t.Fatal("repeated probe produced a different decision")
			}
			if len(v.ProbeWarnings) > 0 {
				recovered++
			}
			if v.Valid {
				accepted++
			} else {
				rejected++
			}
			if err := service.applyValidation(t.Context(), client, file, v, path); err != nil {
				preflightBlocked++
				t.Logf("preflight_blocked=true category=%s", liveErrorCategory(err))
				return
			}
			payload := arr.WebhookPayload{EventType: "Download"}
			if kind == "sonarr" {
				payload.EpisodeFile = &arr.WebhookFile{ID: id}
			} else {
				payload.MovieFile = &arr.WebhookFile{ID: id}
				payload.EventType = "ImportComplete"
			}
			if origin, _, err := service.findOrigin(t.Context(), client, file); err != nil {
				t.Fatalf("webhook origin read: %s", liveErrorCategory(err))
			} else {
				payload.DownloadID = origin
			}
			if err := service.processWebhook(t.Context(), client, payload); err != nil {
				t.Fatalf("webhook dry run: %s", liveErrorCategory(err))
			}
			t.Logf("valid=%t subtitles=%t English=%t unidentified=%t ignored_non_subtitle_diagnostic=%t repeated_probe=matched preflight=passed webhook=passed", v.Valid, v.HasSubtitles, v.HasEnglish, v.HasUnknownLanguage, len(v.ProbeWarnings) > 0)
		})
	}
	if expectedPath != "" && !exampleFound {
		t.Error("the reported example was not among the supplied IDs")
	}
	var reads int64
	for _, rt := range transports {
		reads += rt.reads.Load()
	}
	state := service.state.state
	if len(state.Attempts)+len(state.Operations)+len(state.Completed)+len(state.Webhooks) != 0 {
		t.Fatal("dry run changed retry state")
	}
	t.Logf("files=%d accepted=%d rejected=%d probe_blocked=%d ignored_non_subtitle=%d preflight_blocked=%d reported_example_found=%t GET_requests=%d mutations=0 retry_writes=0", len(values), accepted, rejected, blocked, recovered, preflightBlocked, exampleFound, reads)
}

func liveSonarrProbeFileID(t *testing.T, client *arr.Client, path string) int {
	t.Helper()
	if client == nil {
		t.Fatal("Sonarr is required for lookup by reported path")
	}
	series, err := client.Series(t.Context())
	if err != nil {
		t.Fatalf("series lookup: %s", liveErrorCategory(err))
	}
	var owner *arr.Series
	for _, candidate := range series {
		if candidate.ID > 0 && candidate.Path != "" && strings.HasPrefix(path, strings.TrimRight(candidate.Path, "/")+"/") {
			if owner != nil {
				t.Fatal("ambiguous series ownership for reported path")
			}
			owner = &candidate
		}
	}
	if owner == nil {
		t.Fatal("no series owns the reported path")
	}
	files, err := client.EpisodeFiles(t.Context(), owner.ID)
	if err != nil {
		t.Fatalf("file lookup: %s", liveErrorCategory(err))
	}
	id := 0
	for _, file := range files {
		if file.Path == path {
			if id != 0 || file.ID < 1 || file.ParentID != owner.ID {
				t.Fatal("ambiguous file identity for reported path")
			}
			id = file.ID
		}
	}
	if id == 0 {
		t.Fatal("reported file is no longer in the Arr library")
	}
	return id
}
