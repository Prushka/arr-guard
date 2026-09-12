package guard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
)

// Only the in-memory budget is exhausted to exercise the fallback. Production
// state, configuration, commands and media are never written by this test.
func TestLiveMatchedIDManualImportDryRun(t *testing.T) {
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" || os.Getenv("ARR_LIVE_MANUAL_IMPORT") != "1" {
		t.Skip("requires explicit GET-only manual-import preview opt-in")
	}
	cfg := loadLiveConfig(t)
	if raw := os.Getenv("ARR_LIVE_DOWNLOAD_MAPPINGS_JSON"); raw != "" {
		var mappings []config.PathMapping
		if json.Unmarshal([]byte(raw), &mappings) != nil {
			t.Fatal("invalid temporary download path mappings")
		}
		cfg.PathMappings = append(cfg.PathMappings, mappings...)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Service{config: cfg, log: log, state: &StateStore{state: State{Attempts: map[string]int{}}}, arr: map[string]*arr.Client{}}
	s.probe.Path = cfg.FFprobePath
	transports := map[string]*liveReadTransport{}
	for _, ac := range []*config.Arr{cfg.Sonarr, cfg.Radarr} {
		if ac == nil {
			continue
		}
		base, err := url.Parse(ac.URL)
		if err != nil {
			t.Fatal("invalid configured URL")
		}
		rt := &liveReadTransport{base: base}
		c := arr.NewClient(*ac, log)
		c.EnforceReadOnly()
		c.SetTransport(rt)
		s.arr[ac.Kind], transports[ac.Kind] = c, rt
	}
	for _, kind := range []string{"sonarr", "radarr"} {
		c := s.arr[kind]
		if c == nil {
			continue
		}
		queue, err := c.Queue(t.Context())
		if err != nil {
			t.Fatalf("%s queue: %s", kind, liveErrorCategory(err))
		}
		seen := map[string]bool{}
		previews, approved := 0, 0
		for _, item := range queue {
			if !item.AllowsMatchedIDImport(kind) || seen[strings.ToLower(item.DownloadID)] {
				continue
			}
			seen[strings.ToLower(item.DownloadID)] = true
			plan, err := s.planQueueRecovery(t.Context(), c, item, map[string]bool{})
			if err != nil {
				t.Logf("arr=%s queue_id=%d preflight=%s", kind, item.ID, liveQueueRecoveryError(err))
				continue
			}
			if !plan.search {
				continue
			}
			keys := retryKeys(kind, arr.MediaFile{MovieID: plan.subjectID, ParentID: plan.subjectID}, plan.searchIDs)
			for _, key := range keys {
				s.state.state.Attempts[key] = cfg.MaxAttempts
			}
			before, _ := json.Marshal(s.state.state)
			var captured bytes.Buffer
			s.log = slog.New(slog.NewTextHandler(&captured, nil))
			err = s.recoverBlockedQueueItem(t.Context(), c, item)
			outcome := "approved"
			if err != nil {
				outcome = liveManualImportError(err)
			} else if !strings.Contains(captured.String(), "exhausted matched-by-ID import planned") {
				outcome = "obsolete"
			} else {
				approved++
			}
			after, _ := json.Marshal(s.state.state)
			if !bytes.Equal(before, after) {
				t.Fatal("manual-import dry run changed state")
			}
			previews++
			t.Logf("arr=%s queue_id=%d simulated_exhausted_budget=true preview=%s", kind, item.ID, outcome)
		}
		t.Logf("arr=%s matched_downloads=%d previews=%d approved=%d", kind, len(seen), previews, approved)
	}
	for kind, rt := range transports {
		t.Logf("arr=%s GET_requests=%d mutations=0 retry_writes=0 media_writes=0", kind, rt.reads.Load())
	}
}

func liveManualImportError(err error) string {
	for _, category := range []string{
		"manual import source failed subtitle validation", "manual import candidate has an additional rejection",
		"manual import filename does not independently identify", "manual import filename episode mapping",
		"manual import filename title is unknown or ambiguous", "manual import filename title/year is unknown or ambiguous",
		"manual import candidates do not uniquely cover", "manual import download is shared",
		"manual import requires only", "manual import source resolves outside", "media path is outside configured mappings",
		"manual import candidate quality is unknown", "manual import candidate language metadata is missing",
		"manual import candidate has invalid identity", "manual import candidate rejection inventory is missing",
	} {
		if strings.Contains(err.Error(), category) {
			return category
		}
	}
	return liveErrorCategory(err)
}
