package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Real queue/history/media metadata only. Dry run cannot remove client tasks or
// files; local HTTP fixtures verify removal flags and library preservation.
func TestLiveQueueRecovery(t *testing.T) {
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" || os.Getenv("ARR_LIVE_QUEUE_RECOVERY") != "1" {
		t.Skip("requires GET-only and queue-recovery opt-ins")
	}
	cfg := loadLiveConfig(t)
	for _, ac := range []*ArrConfig{cfg.Sonarr, cfg.Radarr} {
		if ac == nil {
			continue
		}
		t.Run(ac.Kind, func(t *testing.T) {
			base, err := url.Parse(ac.URL)
			if err != nil {
				t.Fatal("invalid configured URL")
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			c := NewArrClient(*ac, log)
			c.readOnly = true
			rt := &liveReadTransport{base: base}
			c.client.Transport = rt
			c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			queue, err := c.Queue(t.Context())
			if err != nil {
				t.Fatalf("queue read: %s", liveErrorCategory(err))
			}
			state := &StateStore{state: State{Attempts: map[string]int{}}}
			s := &Service{config: cfg, log: log, state: state}
			before, _ := json.Marshal(state.state)
			seen, searched := map[string]bool{}, map[string]bool{}
			categories := map[string]int{}
			candidates, planned, partial, existingOnly, searches, skipped := 0, 0, 0, 0, 0, 0
			for _, item := range queue {
				if !item.needsImportRecovery() {
					skipped++
					continue
				}
				candidates++
				origin := strings.ToLower(item.DownloadID)
				if seen[origin] {
					continue
				}
				seen[origin] = true
				plan, err := s.planQueueRecovery(t.Context(), c, item, searched)
				if err != nil {
					category := liveQueueRecoveryError(err)
					categories[category]++
					t.Logf("queue_id=%d preflight_blocked=%s", item.ID, category)
					continue
				}
				if len(plan.rows) == 0 {
					categories["obsolete"]++
					continue
				}
				var captured bytes.Buffer
				s.log = slog.New(slog.NewTextHandler(&captured, nil))
				if err := s.recoverQueueItem(t.Context(), c, item, searched); err != nil {
					category := liveQueueRecoveryError(err)
					categories[category]++
					t.Logf("queue_id=%d fresh_preflight_blocked=%s", item.ID, category)
					continue
				}
				if !strings.Contains(captured.String(), "blocked download recovery planned") {
					categories["obsolete"]++
					continue
				}
				if !strings.Contains(captured.String(), "remove_from_client=true") {
					t.Fatal("recovery plan omitted client task/file removal")
				}
				planned++
				if plan.search {
					searches++
				}
				remaining, needed, err := s.remainingSearchTargets(t.Context(), c, plan.subjectID, plan.episodeIDs)
				if err != nil {
					t.Fatalf("target recheck: %s", liveQueueRecoveryError(err))
				}
				isPartial := ac.Kind == "sonarr" && needed && len(remaining) < len(plan.episodeIDs)
				if isPartial {
					partial++
				}
				if !needed {
					existingOnly++
				}
				t.Logf("queue_id=%d targets=%d missing=%d partial=%t search=%t search_episode_ids=%v remove_from_client=true dry_run=passed", item.ID, len(plan.episodeIDs), len(remaining), isPartial, plan.search, plan.searchIDs)
			}
			after, _ := json.Marshal(state.state)
			if !bytes.Equal(before, after) {
				t.Fatal("dry-run retry state changed")
			}
			t.Logf("queue_rows=%d candidate_rows=%d candidate_downloads=%d planned=%d partial=%d all_existing=%d planned_searches=%d skipped_rows=%d blocked_categories=%v GET_requests=%d mutations=0 retry_writes=0 media_writes=0", len(queue), candidates, len(seen), planned, partial, existingOnly, searches, skipped, categories, rt.reads.Load())
		})
	}
}

func liveQueueRecoveryError(err error) string {
	for _, known := range []string{"queue/download identity is missing", "queue ID was reassigned", "shared download still has active or importable items", "shared download has ambiguous subject mapping", "queue subject changed since scan", "queue history crosses download or subject boundaries", "queue history has incomplete episode mapping", "queue recovery needs grabbed history", "shared queue item has no episode mapping", "imported queue row has no managed file", "queue group changed", "search target no longer exists"} {
		if strings.Contains(err.Error(), known) {
			return known
		}
	}
	return liveErrorCategory(err)
}
