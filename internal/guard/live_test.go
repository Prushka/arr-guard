package guard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/probe"
	"github.com/Prushka/arr-guard/internal/testutil"
)

// liveReadTransport is independent of application dry-run checks. It only permits
// known read endpoints at the configured origin; no redirect or mutation can pass.
type liveReadTransport struct {
	base  *url.URL
	reads atomic.Int64
	trace func(string, time.Duration)
}

func (rt *liveReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := strings.TrimRight(rt.base.Path, "/") + "/api/v3/"
	rel := strings.TrimPrefix(r.URL.Path, prefix)
	allowed := map[string]bool{"system/status": true, "config/downloadclient": true, "series": true, "movie": true, "episode": true, "episodefile": true, "moviefile": true, "queue": true, "history": true, "history/movie": true, "history/series": true, "manualimport": true, "parse": true}
	resource, suffix, _ := strings.Cut(rel, "/")
	if resource == "movie" || resource == "episodefile" || resource == "moviefile" || resource == "command" {
		if id, err := strconv.Atoi(suffix); err == nil && id > 0 {
			allowed[rel] = true
		}
	}
	if r.Method != http.MethodGet || r.URL.Scheme != rt.base.Scheme || r.URL.Host != rt.base.Host || !strings.HasPrefix(r.URL.Path, prefix) || !allowed[rel] {
		return nil, fmt.Errorf("live safety transport blocked request")
	}
	rt.reads.Add(1)
	started := time.Now()
	response, err := http.DefaultTransport.RoundTrip(r)
	if rt.trace != nil {
		if err != nil {
			rt.trace(rel, time.Since(started))
		} else {
			response.Body = &liveTimedBody{ReadCloser: response.Body, done: func() { rt.trace(rel, time.Since(started)) }}
		}
	}
	return response, err
}

type liveTimedBody struct {
	io.ReadCloser
	done func()
}

func (b *liveTimedBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

// Opt-in diagnosis for known Sonarr file IDs. Submit concurrent candidates with
// the normal HTTP deadline to verify preflight serialization on the live server.
func TestLiveTargetedPreflight(t *testing.T) {
	raw := os.Getenv("ARR_LIVE_PREFLIGHT_IDS")
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" || raw == "" {
		t.Skip("requires read-only opt-in and explicit numeric Sonarr file IDs")
	}
	values := strings.Split(raw, ",")
	if len(values) > 20 {
		t.Fatal("at most 20 diagnostic file IDs are allowed")
	}
	var ids []int
	for _, value := range values {
		id, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || id < 1 {
			t.Fatal("diagnostic file IDs must be positive integers")
		}
		ids = append(ids, id)
	}
	cfg := loadLiveConfig(t)
	if cfg.Sonarr == nil {
		t.Fatal("Sonarr is not configured")
	}
	base, err := url.Parse(cfg.Sonarr.URL)
	if err != nil {
		t.Fatal("invalid server URL")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := &liveReadTransport{base: base}
	rt.trace = func(endpoint string, elapsed time.Duration) {
		if elapsed >= time.Second {
			t.Logf("slow_GET endpoint=%s elapsed=%s", endpoint, elapsed.Round(time.Millisecond))
		}
	}
	c := arr.NewClient(*cfg.Sonarr, log)
	c.EnforceReadOnly()
	c.SetTransport(rt)
	s := &Service{config: cfg, log: log, state: &StateStore{state: State{Attempts: map[string]int{}}}, arr: map[string]*arr.Client{"sonarr": c}, probe: probe.Prober{Path: cfg.FFprobePath, Timeout: 2 * time.Minute}}
	check := func(id int) {
		file, err := c.GetMediaFile(t.Context(), id)
		if err != nil {
			t.Errorf("file_id=%d resource_read=%s", id, liveErrorCategory(err))
			return
		}
		file.Year, err = c.EpisodeReleaseYearForFile(t.Context(), file.ParentID, file.ID)
		if err != nil {
			t.Errorf("file_id=%d episode_read=%s", id, liveErrorCategory(err))
			return
		}
		v, path, err := s.validate(t.Context(), file)
		if err != nil {
			t.Errorf("file_id=%d probe=%s", id, liveErrorCategory(err))
			return
		}
		started := time.Now()
		err = s.applyValidation(t.Context(), c, file, v, path)
		category := "passed"
		if err != nil {
			category = liveErrorCategory(err)
		}
		t.Logf("file_id=%d valid=%t preflight=%s preflight_elapsed=%s HTTP_timeout=%s mutations=0", id, v.Valid, category, time.Since(started).Round(time.Millisecond), c.RequestTimeout())
		if errors.Is(err, context.DeadlineExceeded) {
			t.Error("serialized read-only preflight timed out")
		}
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, id := range arr.CanonicalIDs(ids) {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			check(id)
		})
	}
	wg.Wait()
}

func loadLiveConfig(t *testing.T) config.Config {
	t.Helper()
	// Preserve root-relative .env settings after moving tests into this package.
	t.Chdir(testutil.RepositoryRoot(t))
	f, err := os.Open(".env")
	if err != nil {
		t.Fatal("cannot read local .env")
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			t.Fatal("invalid .env entry (value redacted)")
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		t.Setenv(strings.TrimSpace(name), value)
	}
	if scanner.Err() != nil {
		t.Fatal("cannot parse .env")
	}
	t.Setenv("DRY_RUN", "true")
	t.Setenv("MODE", "subtitles")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal("invalid live configuration (details redacted)")
	}
	return cfg
}

func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("ARR_LIVE_READ_ONLY") != "1" {
		t.Skip("opt in with ARR_LIVE_READ_ONLY=1; GET-only, no media writes")
	}
	cfg := loadLiveConfig(t)
	workers := 4
	if raw := os.Getenv("ARR_LIVE_WORKERS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 8 {
			t.Fatal("ARR_LIVE_WORKERS must be between 1 and 8")
		}
		workers = parsed
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	allClients := map[string]*arr.Client{}
	for _, ac := range []*config.Arr{cfg.Sonarr, cfg.Radarr} {
		if ac == nil {
			continue
		}
		t.Run(ac.Kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Minute)
			defer cancel()
			base, err := url.Parse(ac.URL)
			if err != nil {
				t.Fatal("invalid server URL")
			}
			rt := &liveReadTransport{base: base}
			client := arr.NewClient(*ac, log)
			client.EnforceReadOnly()
			allClients[ac.Kind] = client
			client.SetTransport(rt)
			if err := client.Test(ctx); err != nil {
				t.Fatal("server status read failed (details redacted)")
			}
			files, err := client.ListSubtitleGuardFiles(ctx)
			if err != nil {
				t.Fatal("library read failed (details redacted)")
			}
			queue, err := client.Queue(ctx)
			if err != nil {
				t.Fatal("queue read failed (details redacted)")
			}
			service := &Service{config: cfg, log: log, state: &StateStore{state: State{Attempts: map[string]int{}}}, arr: map[string]*arr.Client{ac.Kind: client}, probe: probe.Prober{Path: cfg.FFprobePath, Timeout: 2 * time.Minute}}
			accessible, blocked := 0, 0
			sizeMismatches := 0
			for _, file := range files {
				if file.ID < 1 || file.SubjectID(ac.Kind) < 1 || file.Path == "" {
					t.Fatal("library contains invalid file identity")
				}
				if info, err := os.Stat(service.mapPath(file.Path)); err == nil && info.Mode().IsRegular() {
					accessible++
					if file.Size > 0 && file.Size != info.Size() {
						sizeMismatches++
					}
				}
			}
			for _, item := range queue {
				if item.NeedsImportRecovery() {
					blocked++
				}
			}
			if err := service.recoverBlockedQueue(ctx, client); err != nil {
				t.Error("queue dry run failed (details redacted)")
			}
			if os.Getenv("ARR_LIVE_ORIGIN_DIAGNOSTICS") == "1" {
				liveDiagnoseQueuedOrigins(t, ctx, service, client, files, queue)
			}
			if len(files) > 0 {
				file := files[0]
				if _, err := client.GetMediaFile(ctx, file.ID); err != nil {
					t.Error("single file read failed")
				}
				if _, err := client.SubjectHistory(ctx, file.SubjectID(ac.Kind)); err != nil {
					t.Error("subject history read failed")
				}
			}
			if os.Getenv("ARR_LIVE_WEBHOOK_CHECK") == "1" {
				for _, file := range files {
					if shouldSkipSilentMedia(file.Year, time.Now()) {
						continue
					}
					payload := arr.WebhookPayload{EventType: "Download"}
					if ac.Kind == "sonarr" {
						payload.EpisodeFile = &arr.WebhookFile{ID: file.ID}
						payload.Series = &arr.Series{ID: file.ParentID}
					} else {
						payload.MovieFile = &arr.WebhookFile{ID: file.ID}
						payload.Movie = &arr.Movie{ID: file.MovieID}
					}
					if origin, _, err := service.findOrigin(ctx, client, file); err == nil {
						payload.DownloadID = origin
					}
					if err := service.processWebhook(ctx, client, payload); err != nil {
						t.Logf("real_webhook_dry_run=safety_blocked category=%s", liveErrorCategory(err))
					} else {
						t.Log("real_webhook_dry_run=passed")
					}
					v, path, err := service.validate(ctx, file)
					if err != nil {
						t.Errorf("snapshot-check probe failed: %s", liveErrorCategory(err))
					} else if v.Valid {
						// Replace only the in-memory snapshot with a different local
						// directory. Never touch the real media or subtitle directory.
						v.DirectoryInfo, err = os.Stat(t.TempDir())
						if err != nil {
							t.Fatal("cannot prepare local snapshot fixture")
						}
						if err := service.applyValidation(ctx, client, file, v, path); err == nil || !strings.Contains(err.Error(), "subtitle directory changed") {
							t.Error("live stale-directory check did not refuse the snapshot")
						} else {
							t.Log("real_snapshot_recheck=passed local_memory_fault_only=true")
						}
					} else {
						t.Log("LIMITATION: first webhook sample is invalid; valid-file snapshot refusal was covered locally")
					}
					break
				}
			}
			if os.Getenv("ARR_LIVE_FULL_SCAN") == "1" {
				var validated, valid, invalid, skipped, probeErrors, preflightErrors atomic.Int64
				var processed atomic.Int64
				categories := map[string]int{}
				var categoryMu sync.Mutex
				countError := func(stage string, id int, err error) {
					category := liveErrorCategory(err)
					categoryMu.Lock()
					categories[category]++
					categoryMu.Unlock()
					t.Logf("file_id=%d stage=%s category=%s", id, stage, category)
				}
				jobs := make(chan arr.MediaFile)
				var wg sync.WaitGroup
				for i := 0; i < workers; i++ {
					wg.Go(func() {
						for file := range jobs {
							if n := processed.Add(1); n%500 == 0 {
								t.Logf("progress files=%d/%d GET_requests=%d", n, len(files), rt.reads.Load())
							}
							if shouldSkipSilentMedia(file.Year, time.Now()) {
								skipped.Add(1)
								continue
							}
							v, path, err := service.validate(ctx, file)
							if err != nil {
								probeErrors.Add(1)
								countError("probe", file.ID, err)
								continue
							}
							validated.Add(1)
							if v.Valid {
								valid.Add(1)
							} else {
								invalid.Add(1)
							}
							if err := service.applyValidation(ctx, client, file, v, path); err != nil {
								preflightErrors.Add(1)
								countError("preflight", file.ID, err)
							}
						}
					})
				}
				for _, file := range files {
					select {
					case jobs <- file:
					case <-ctx.Done():
					}
				}
				close(jobs)
				wg.Wait()
				t.Logf("error_categories=%v", categories)
				t.Logf("full_subtitle_scan validated=%d valid=%d invalid=%d age_skipped=%d probe_errors=%d safety_preflight_blocks=%d", validated.Load(), valid.Load(), invalid.Load(), skipped.Load(), probeErrors.Load(), preflightErrors.Load())
				// This local report uses a disposable output directory, never .env's
				// report location or any media path. The scan is read-only on media.
				if err := service.CleanupPending(ctx); err != nil {
					t.Error("dry-run cleanup failed")
				}
				if len(service.state.Pending()) != 0 || len(service.state.state.Attempts) != 0 {
					t.Error("dry-run changed retry state")
				}
				if err := ctx.Err(); err != nil {
					t.Error(err)
				}
				if probeErrors.Load() > 0 {
					t.Log("LIMITATION: media with failed probes was left untouched; not every file could be validated")
				}
			}
			t.Logf("library_files=%d accessible_media=%d size_mismatches=%d queue_items=%d blocked_items=%d GET_requests=%d mutations=0", len(files), accessible, sizeMismatches, len(queue), blocked, rt.reads.Load())
			if accessible != len(files) {
				t.Log("LIMITATION: mapped server media is inaccessible here; real subtitle/orphan scans require a read-only mount")
			}
		})
	}
	if os.Getenv("ARR_LIVE_FULL_SCAN") == "1" {
		cfg.UnmatchedPath = filepath.Join(t.TempDir(), "unmatched.json")
		service := &Service{config: cfg, log: log, arr: allClients}
		if err := service.ScanUnmatched(t.Context()); err != nil {
			t.Errorf("combined orphan scan failed: %s", liveErrorCategory(err))
		} else {
			data, err := os.ReadFile(cfg.UnmatchedPath)
			if err != nil {
				t.Fatal("cannot read temporary orphan report")
			}
			var report UnmatchedReport
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal("invalid temporary orphan report")
			}
			t.Logf("combined_orphan_scan roots=%d unmatched_files=%d local_temporary_report_only=true", len(report.Roots), len(report.Files))
		}
	}
}

func liveErrorCategory(err error) string {
	var apiErr *arr.HTTPError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("HTTP %d", apiErr.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network timeout"
	}
	for _, category := range []string{"automatic failed-redownload", "shared with another subject", "episodes without imported files", "ambiguous originating import history", "pagination", "oversized or null", "no content", "deadline exceeded", "still queued", "still importing", "filter mismatch", "crosses subject", "no grabbed history", "changed", "not assigned", "mismatched", "does not match", "missing the streams array", "ffprobe reported media errors", "timed out", "interrupted", "not a regular file", "empty", "outside configured", "HTTP 404", "HTTP 401", "HTTP 403", "HTTP 429", "HTTP 500", "cannot find", "Access is denied", "ffprobe:"} {
		if strings.Contains(err.Error(), category) {
			return category
		}
	}
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	return fmt.Sprintf("other read/probe failure (%T)", err)
}

// Inspect imported queue groups, then probe only files with those protected
// origins. All requests still use liveReadTransport; no server state is changed.
func liveDiagnoseQueuedOrigins(t *testing.T, ctx context.Context, s *Service, c *arr.Client, files []arr.MediaFile, queue []arr.QueueRecord) {
	t.Helper()
	byID, bySubject := map[int]arr.MediaFile{}, map[int]arr.MediaFile{}
	for _, file := range files {
		byID[file.ID] = file
		bySubject[file.SubjectID(c.Kind())] = file
	}
	seen, protected := map[string]bool{}, map[string]bool{}
	subjects, candidates := map[int]bool{}, map[int]bool{}
	groupCategories, fileCategories := map[string]int{}, map[string]int{}
	for _, q := range queue {
		origin := strings.ToLower(q.DownloadID)
		if origin == "" || seen[origin] || !strings.EqualFold(q.Status, "completed") || !strings.EqualFold(q.TrackedDownloadState, "imported") {
			continue
		}
		seen[origin] = true
		subject := q.MovieID
		if c.Kind() == "sonarr" {
			subject = q.SeriesID
		}
		file, ok := bySubject[subject]
		if !ok {
			continue
		}
		if _, err := s.prepareOrigin(ctx, c, file, q.DownloadID); err != nil {
			groupCategories[liveErrorCategory(err)]++
			protected[origin], subjects[subject] = true, true
		}
	}
	for subject := range subjects {
		history, err := c.SubjectHistory(ctx, subject)
		if err != nil {
			t.Errorf("origin diagnostic history read failed: %s", liveErrorCategory(err))
			continue
		}
		for _, h := range history {
			if !strings.EqualFold(h.EventType, "downloadFolderImported") || !protected[strings.ToLower(h.DownloadID)] {
				continue
			}
			id, err := strconv.Atoi(historyData(h, "fileId"))
			if err == nil && byID[id].SubjectID(c.Kind()) == subject {
				candidates[id] = true
			}
		}
	}
	// Also look for ambiguous imported-file history, which can protect a file
	// whose download is no longer represented in the current queue.
	ambiguous := map[int]bool{}
	if c.Kind() == "sonarr" {
		for subject := range bySubject {
			history, err := c.SubjectHistory(ctx, subject)
			if err != nil {
				groupCategories[liveErrorCategory(err)]++
				continue
			}
			origins := map[int]string{}
			for _, h := range history {
				if !strings.EqualFold(h.EventType, "downloadFolderImported") {
					continue
				}
				id, err := strconv.Atoi(historyData(h, "fileId"))
				if err != nil || byID[id].SubjectID(c.Kind()) != subject {
					continue
				}
				if previous, exists := origins[id]; exists && !strings.EqualFold(previous, h.DownloadID) {
					ambiguous[id], candidates[id] = true, true
				}
				origins[id] = h.DownloadID
			}
		}
	}
	invalid := 0
	for id := range candidates {
		file := byID[id]
		if shouldSkipSilentMedia(file.Year, time.Now()) {
			continue
		}
		v, path, err := s.validate(ctx, file)
		if err != nil {
			fileCategories[liveErrorCategory(err)]++
			continue
		}
		if !v.Valid {
			invalid++
			if err := s.applyValidation(ctx, c, file, v, path); err != nil {
				fileCategories[liveErrorCategory(err)]++
			}
		}
	}
	t.Logf("origin_diagnostics groups=%d group_blocks=%v ambiguous_history_files=%d candidate_files=%d invalid_files=%d file_blocks=%v", len(seen), groupCategories, len(ambiguous), len(candidates), invalid, fileCategories)
}
