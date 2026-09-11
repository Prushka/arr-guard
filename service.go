package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	config       Config
	log          *slog.Logger
	probe        Prober
	probeFn      func(context.Context, string) (Validation, error)
	state        *StateStore
	arr          map[string]*ArrClient
	jobs         chan webhookJob
	stop         chan struct{}
	wg           sync.WaitGroup
	locksMu      sync.Mutex
	locks        map[string]*fileLock
	handledMu    sync.Mutex
	handled      map[string]time.Time
	stopOnce     sync.Once
	mutationMu   sync.Mutex
	workerCancel context.CancelFunc
	enqueueMu    sync.Mutex
	stopping     bool
	scheduled    map[string]bool
}

// fileLock tracks waiters so an idle lock can be removed without allowing
// another webhook to create a second lock for the same media file.
type fileLock struct {
	mu   sync.Mutex
	refs int
}

type webhookJob struct {
	key     string
	client  *ArrClient
	payload WebhookPayload
}

const blockedQueueScanInterval = time.Hour

const silentMediaSkipAfterYears = 50

// Keep successful webhook IDs briefly to absorb Arr retries while bounding
// memory use. Failed processing removes the claim immediately for retry.
const webhookDedupTTL = 24 * time.Hour

func NewService(config Config, log *slog.Logger) (*Service, error) {
	state, err := openServiceState(config)
	if err != nil {
		return nil, err
	}
	service := &Service{
		config:  config,
		log:     log,
		probe:   Prober{Path: config.FFprobePath, Timeout: 10 * time.Minute},
		state:   state,
		arr:     make(map[string]*ArrClient),
		jobs:    make(chan webhookJob, config.Workers*4),
		stop:    make(chan struct{}),
		locks:   make(map[string]*fileLock),
		handled: make(map[string]time.Time),
	}
	if config.Sonarr != nil {
		service.arr["sonarr"] = NewArrClient(*config.Sonarr, log)
	}
	if config.Radarr != nil {
		service.arr["radarr"] = NewArrClient(*config.Radarr, log)
	}
	for _, client := range service.arr {
		client.readOnly = config.DryRun || config.Mode == "unmatched"
	}
	if !config.DryRun && config.Mode != "unmatched" {
		if err := state.Bind(service.arr); err != nil {
			_ = state.Close()
			return nil, err
		}
	}
	return service, nil
}

func (s *Service) StartWorkers(ctx context.Context) {
	ctx, s.workerCancel = context.WithCancel(ctx)
	for i := 0; i < s.config.Workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case item := <-s.jobs:
					jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
					err := s.processWebhook(jobCtx, item.client, item.payload)
					if err != nil && !errors.Is(err, context.Canceled) {
						s.log.Error("webhook processing failed", "error", err, "arr", item.client.Kind())
					}
					cancel()
					s.finishJob(item, err)
				case <-s.stop:
					return
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			s.dispatchStoredJobs()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runBlockedQueueScan(ctx)

		ticker := time.NewTicker(blockedQueueScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanupWebhookState()
				s.runBlockedQueueScan(ctx)
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Service) StopWorkers() {
	s.enqueueMu.Lock()
	s.stopping = true
	s.enqueueMu.Unlock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// CleanupPending reports unfinished operations without replaying mutations. Arr
// does not provide idempotency keys; even an HTTP error may follow a committed
// mutation. The durable journal requires operator reconciliation in this case.
func (s *Service) CleanupPending(_ context.Context) error {
	if s.config.DryRun || s.state == nil {
		return nil
	}
	pending := s.state.Pending()
	if len(pending) == 0 {
		return nil
	}
	for key, op := range pending {
		s.log.Error("unfinished operation requires reconciliation", "key", key, "phase", op.Phase)
	}
	return fmt.Errorf("%d unfinished operations retained in STATE_PATH; automatic replay disabled", len(pending))
}

func (s *Service) runBlockedQueueScan(ctx context.Context) {
	if !s.config.RecoverBlockedQueue {
		return
	}
	for _, client := range s.arr {
		if err := s.recoverBlockedQueue(ctx, client); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("blocked queue scan failed", "arr", client.Kind(), "error", err)
		}
	}
}

func (s *Service) recoverBlockedQueue(ctx context.Context, client *ArrClient) error {
	queue, err := client.Queue(ctx)
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}

	var firstErr error
	recovered := 0
	searched := map[string]bool{}
	seen := map[string]bool{}
	for _, item := range queue {
		if !item.needsImportRecovery() {
			continue
		}
		key := strings.ToLower(item.DownloadID)
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := s.recoverQueueItem(ctx, client, item, searched); err != nil {
			s.log.Error("blocked queue item recovery failed", "arr", client.Kind(), "queue_id", item.ID, "download_id", item.DownloadID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recovered++
	}
	s.log.Info("blocked queue scan complete", "arr", client.Kind(), "queue_items", len(queue), "recovered", recovered)
	return firstErr
}

func (s *Service) Enqueue(client *ArrClient, payload WebhookPayload) error {
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	if s.stopping {
		return errors.New("service is stopping")
	}
	if !s.config.DryRun && s.state != nil {
		key, stored, err := storedWebhook(client.Kind(), payload)
		if err != nil {
			return err
		}
		return s.state.AddWebhook(key, stored)
	}
	select {
	case s.jobs <- webhookJob{client: client, payload: payload}:
		return nil
	default:
		return errors.New("webhook queue is full")
	}
}

func (s *Service) processWebhook(ctx context.Context, client *ArrClient, payload WebhookPayload) error {
	if !strings.EqualFold(payload.EventType, "download") && !strings.EqualFold(payload.EventType, "importcomplete") {
		return nil
	}
	files := payload.files(client.Kind())
	if len(files) == 0 {
		return errors.New("download webhook contains no media file")
	}
	var errs []error
	for _, wf := range files {
		if wf.ID < 1 {
			errs = append(errs, errors.New("webhook media ID must be positive"))
			continue
		}
		key := operationKey(client.Kind(), wf.ID)
		lock := s.acquireFileLock(key)
		if !s.claimWebhook(key) {
			s.releaseFileLock(key, lock)
			continue
		}
		err := func() error {
			file, err := client.GetMediaFile(ctx, wf.ID)
			if err != nil {
				var apiErr *ArrHTTPError
				if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
					return nil
				}
				return err
			}
			if client.Kind() == "sonarr" {
				if payload.Series != nil && payload.Series.ID != file.ParentID {
					return errors.New("webhook series does not own file")
				}
				file.Year, err = client.EpisodeReleaseYearForFile(ctx, file.ParentID, file.ID)
			} else {
				if payload.Movie != nil && payload.Movie.ID != file.MovieID {
					return errors.New("webhook movie does not own file")
				}
				var movie Movie
				err = client.do(ctx, http.MethodGet, client.apiPath("movie", strconv.Itoa(file.MovieID)), nil, nil, &movie)
				if err == nil && movie.ID != file.MovieID {
					return errors.New("movie metadata identity mismatch")
				}
				file.Year = movie.Year
			}
			if err != nil {
				return err
			}
			// Paths, release dates, episodes and origin are always resolved from Arr.
			return s.auditFileWithOrigin(ctx, client, file, payload.DownloadID)
		}()
		s.finishWebhook(key, err)
		s.releaseFileLock(key, lock)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p WebhookPayload) files(kind string) []WebhookFile {
	if kind == "sonarr" {
		if len(p.EpisodeFiles) > 0 {
			return p.EpisodeFiles
		}
		if p.EpisodeFile != nil {
			return []WebhookFile{*p.EpisodeFile}
		}
		return nil
	}
	if len(p.MovieFiles) > 0 {
		return p.MovieFiles
	}
	if p.MovieFile != nil {
		return []WebhookFile{*p.MovieFile}
	}
	return nil
}

func (s *Service) claimWebhook(key string) bool {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	if s.handled == nil {
		s.handled = make(map[string]time.Time)
	}
	now := time.Now()
	s.cleanupWebhookStateLocked(now)
	if handledAt, ok := s.handled[key]; ok && now.Sub(handledAt) < webhookDedupTTL {
		return false
	}
	s.handled[key] = now
	return true
}

func (s *Service) cleanupWebhookState() {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	s.cleanupWebhookStateLocked(time.Now())
}

func (s *Service) cleanupWebhookStateLocked(now time.Time) {
	for handledKey, handledAt := range s.handled {
		if now.Sub(handledAt) >= webhookDedupTTL {
			delete(s.handled, handledKey)
		}
	}
}

func (s *Service) finishWebhook(key string, err error) {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	if err != nil {
		delete(s.handled, key)
		return
	}
	if s.handled == nil {
		s.handled = make(map[string]time.Time)
	}
	s.handled[key] = time.Now()
}

func (s *Service) acquireFileLock(key string) *fileLock {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*fileLock)
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &fileLock{}
		s.locks[key] = lock
	}
	lock.refs++
	s.locksMu.Unlock()
	lock.mu.Lock()
	return lock
}

func (s *Service) releaseFileLock(key string, lock *fileLock) {
	lock.mu.Unlock()
	s.locksMu.Lock()
	lock.refs--
	if lock.refs == 0 && s.locks[key] == lock {
		delete(s.locks, key)
	}
	s.locksMu.Unlock()
}

func (s *Service) Audit(ctx context.Context) error {
	var auditErrors []error
	if s.config.Workers < 1 {
		return errors.New("audit requires at least one worker")
	}
	for _, client := range s.arr {
		var files []MediaFile
		err := retryScanReads(ctx, func() error {
			var readErr error
			files, readErr = client.ListSubtitleGuardFiles(ctx)
			return readErr
		})
		if err != nil {
			auditErrors = append(auditErrors, fmt.Errorf("audit %s: %w", client.Kind(), err))
			continue
		}
		s.log.Info("library scan started", "arr", client.Kind(), "files", len(files))
		sem := make(chan struct{}, s.config.Workers)
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex
		for _, mediaFile := range files {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return ctx.Err()
			}
			file := mediaFile
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := s.auditFileWithRetries(ctx, client, file); err != nil {
					s.log.Warn("library file left untouched or requires reconciliation", "arr", client.Kind(), "file_id", file.ID, "error", err)
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		if firstErr != nil {
			auditErrors = append(auditErrors, fmt.Errorf("audit %s: %w", client.Kind(), firstErr))
		}
		s.log.Info("library scan complete", "arr", client.Kind())
		if s.config.RecoverBlockedQueue {
			if err := s.recoverBlockedQueue(ctx, client); err != nil {
				auditErrors = append(auditErrors, fmt.Errorf("recover %s queue: %w", client.Kind(), err))
			}
		}
	}
	return errors.Join(auditErrors...)
}

// ScanUnmatched lists media files beneath the configured mapped library roots
// that do not have a matching Sonarr or Radarr media-file ID. It intentionally
// does not probe or call applyValidation: an orphan has no Arr media-file ID,
// so subtitle remediation is neither possible nor safe.
func (s *Service) ScanUnmatched(ctx context.Context) error {
	resolvedMappings := make([]PathMapping, 0, len(s.config.PathMappings))
	for _, mapping := range s.config.PathMappings {
		root, err := filepath.EvalSymlinks(mapping.To)
		if err != nil {
			return fmt.Errorf("resolve unmatched scan root: %w", err)
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return errors.New("unmatched scan root is not an accessible directory")
		}
		resolvedMappings = append(resolvedMappings, PathMapping{To: root})
	}
	roots := scanRoots(resolvedMappings)
	if len(roots) == 0 {
		return errors.New("unmatched scan requires at least one PATH_MAPPINGS_JSON destination path")
	}

	matched := make(map[string]struct{})
	for _, client := range s.arr {
		files, err := client.ListLibraryFiles(ctx)
		if err != nil {
			return fmt.Errorf("list %s library files: %w", client.Kind(), err)
		}
		for _, file := range files {
			if file.ID > 0 && file.SubjectID(client.Kind()) > 0 {
				path := s.mapPath(file.Path)
				if path != "" && path != "." {
					matched[scanPathKey(resolveExistingPath(path))] = struct{}{}
				}
			}
		}
	}

	report := UnmatchedReport{
		GeneratedAt: time.Now().UTC(),
		Roots:       roots,
		Files:       make([]UnmatchedMedia, 0),
	}
	scanned := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				if s.isUnmatchedExcludedDir(root, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			if !isMediaPath(path) {
				return nil
			}
			scanned++
			if _, ok := matched[scanPathKey(path)]; ok {
				return nil
			}
			report.Files = append(report.Files, UnmatchedMedia{Path: path})
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan unmatched root %s: %w", root, err)
		}
	}
	sort.Slice(report.Files, func(i, j int) bool { return scanPathKey(report.Files[i].Path) < scanPathKey(report.Files[j].Path) })
	if err := writeUnmatchedReport(s.config.UnmatchedPath, report); err != nil {
		return err
	}
	s.log.Info("unmatched scan complete", "roots", len(roots), "media_files", scanned, "unmatched_files", len(report.Files), "output", s.config.UnmatchedPath)
	return nil
}

func (s *Service) isUnmatchedExcludedDir(root, path string) bool {
	pathKey := scanPathKey(path)
	for _, excluded := range s.config.UnmatchedExcludeDirs {
		excluded = strings.TrimSpace(excluded)
		if excluded == "" {
			continue
		}
		excludedKey := scanPathKey(resolveExistingPath(excluded))
		if !filepath.IsAbs(filepath.Clean(excluded)) {
			excludedKey = scanPathKey(filepath.Join(root, excluded))
		}
		if pathKey == excludedKey {
			return true
		}
	}
	return false
}

func scanRoots(mappings []PathMapping) []string {
	values := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		if root := strings.TrimSpace(mapping.To); root != "" {
			values = append(values, filepath.Clean(root))
		}
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) < len(values[j])
	})
	roots := make([]string, 0, len(values))
	for _, candidate := range values {
		duplicate := false
		for _, root := range roots {
			relative, err := filepath.Rel(root, candidate)
			if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			roots = append(roots, candidate)
		}
	}
	return roots
}

func scanPathKey(value string) string {
	value = filepath.Clean(strings.TrimSpace(value))
	if runtime.GOOS == "windows" {
		return strings.ToLower(value)
	}
	return value
}

var mediaExtensions = map[string]struct{}{
	".264": {}, ".265": {}, ".3g2": {}, ".3gp": {}, ".asf": {}, ".avi": {}, ".divx": {},
	".dvr-ms": {}, ".f4v": {}, ".flv": {}, ".iso": {}, ".m2ts": {},
	".h264": {}, ".h265": {}, ".hevc": {}, ".m3u": {}, ".m3u8": {},
	".m2v": {}, ".m4v": {}, ".mkv": {}, ".mov": {}, ".mp4": {}, ".mpe": {},
	".mpeg": {}, ".mpg": {}, ".mpv2": {}, ".mts": {}, ".mxf": {}, ".ogm": {},
	".ogv": {}, ".rm": {}, ".rmvb": {}, ".ts": {}, ".vob": {},
	".webm": {}, ".wmv": {}, ".wtv": {}, ".xvid": {},
}

func isMediaPath(value string) bool {
	_, ok := mediaExtensions[strings.ToLower(filepath.Ext(value))]
	return ok
}

func writeUnmatchedReport(path string, report UnmatchedReport) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("UNMATCHED_PATH must not be empty")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode unmatched report: %w", err)
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create unmatched report directory: %w", err)
		}
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write unmatched report: %w", err)
	}
	return nil
}

func (s *Service) auditFile(ctx context.Context, client *ArrClient, file MediaFile) error {
	return s.auditFileWithOrigin(ctx, client, file, "")
}

func (s *Service) auditFileWithOrigin(ctx context.Context, client *ArrClient, file MediaFile, expectedDownloadID string) error {
	if s.skipSilentMediaGuard(client, file) {
		return nil
	}
	validation, pathOnDisk, err := s.validate(ctx, file)
	if err != nil {
		return err
	}
	return s.applyValidationWithOrigin(ctx, client, file, validation, pathOnDisk, expectedDownloadID)
}

func (s *Service) findOrigin(ctx context.Context, client *ArrClient, file MediaFile) (string, int, error) {
	records, err := client.SubjectHistory(ctx, file.SubjectID(client.Kind()))
	if err != nil {
		return "", 0, err
	}
	var downloadID string
	var historyID int
	for _, record := range records {
		if strings.EqualFold(record.EventType, "downloadFolderImported") && historyData(record, "fileId") == strconv.Itoa(file.ID) {
			if (client.Kind() == "radarr" && record.MovieID != file.MovieID) || (client.Kind() == "sonarr" && record.SeriesID != file.ParentID) {
				return "", 0, errors.New("import history has mismatched subject identity")
			}
			if historyID != 0 && !strings.EqualFold(downloadID, record.DownloadID) {
				return "", 0, errors.New("file has ambiguous originating import history")
			}
			if record.ID < 1 {
				return "", 0, errors.New("import history has no valid ID")
			}
			downloadID, historyID = record.DownloadID, record.ID
		}
	}
	return downloadID, historyID, nil
}

func historyData(record HistoryRecord, key string) string {
	for recordKey, value := range record.Data {
		if strings.EqualFold(recordKey, key) {
			return value
		}
	}
	return ""
}

func (s *Service) validate(ctx context.Context, file MediaFile) (Validation, string, error) {
	pathOnDisk := s.mapPath(file.Path)
	if file.Path != strings.TrimSpace(file.Path) {
		return Validation{}, pathOnDisk, errors.New("ambiguous whitespace in media path")
	}
	if file.ID < 1 {
		return Validation{}, pathOnDisk, errors.New("media file ID is missing")
	}
	if pathOnDisk == "" || pathOnDisk == "." {
		return Validation{}, pathOnDisk, fmt.Errorf("media file %d has no path", file.ID)
	}
	if len(s.config.PathMappings) > 0 {
		matched := false
		for _, mapping := range s.config.PathMappings {
			value, from := comparableArrPath(file.Path, mapping.From)
			if value == from || strings.HasPrefix(value, strings.TrimRight(from, "/")+"/") {
				matched = true
				break
			}
		}
		if !matched {
			return Validation{}, pathOnDisk, errors.New("media path is outside configured mappings")
		}
	}
	validation, err := s.probePath(ctx, pathOnDisk)
	if err != nil {
		return Validation{}, pathOnDisk, deferProcessing(fmt.Errorf("probe %s: %w", pathOnDisk, err))
	}
	if file.Size > 0 && validation.fileInfo != nil && validation.fileInfo.Size() != file.Size {
		return Validation{}, pathOnDisk, deferProcessing(errors.New("local file size does not match Arr metadata"))
	}
	if len(validation.ProbeWarnings) > 0 {
		s.log.Warn("ffprobe recovered from video diagnostic; applying subtitle policy", "file_id", file.ID, "diagnostics", validation.ProbeWarnings)
	}
	validation = applyOldMediaGrace(validation, file.Year, time.Now())
	return validation, pathOnDisk, nil
}

func (s *Service) probePath(ctx context.Context, path string) (Validation, error) {
	if s.probeFn != nil {
		return s.probeFn(ctx, path)
	}
	return s.probe.Validate(ctx, path)
}

func (s *Service) applyValidation(ctx context.Context, client *ArrClient, file MediaFile, validation Validation, pathOnDisk string) error {
	return s.applyValidationWithOrigin(ctx, client, file, validation, pathOnDisk, "")
}

func (s *Service) applyValidationWithOrigin(ctx context.Context, client *ArrClient, file MediaFile, validation Validation, pathOnDisk, expectedDownloadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Probes can run in parallel. Serialize preflights in both modes so dry-run
	// history reads have the same load as write mode. State transitions and
	// mutations share this lock with queue recovery and webhook processing.
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if file.ID < 1 || file.SubjectID(client.Kind()) < 1 {
		return errors.New("invalid media identity")
	}
	fresh, err := client.GetMediaFile(ctx, file.ID)
	if err != nil {
		return deferMissingMedia(err)
	}
	if !sameMedia(file, fresh, client.Kind()) {
		return deferProcessing(errors.New("media changed since probe; refusing stale validation"))
	}
	var ids []int
	if client.Kind() == "sonarr" {
		ids, err = client.EpisodeIDsForFile(ctx, file.ParentID, file.ID)
		if err != nil {
			return err
		}
		ids = canonicalIDs(ids)
		if len(ids) == 0 {
			return deferProcessing(errors.New("sonarr file is not assigned to any episode"))
		}
	}
	keys := retryKeys(client.Kind(), file, ids)
	if validation.Valid {
		info, err := os.Stat(pathOnDisk)
		if err != nil || !sameDiskFile(validation.fileInfo, info) {
			return deferProcessing(errors.New("valid media changed or has no verified snapshot before retry-state reset"))
		}
		if err := validation.checkSubtitleSnapshot(pathOnDisk); err != nil {
			return fmt.Errorf("before retry-state reset: %w", err)
		}
		if s.config.DryRun {
			return nil
		}
		// Clear per-episode state and legacy composite/path keys on every success,
		// including ordinary library scans, not just webhook jobs.
		keys = append(keys, retryKey(client.Kind(), file, ids, file.RelativePath), retryKey(client.Kind(), file, nil, file.RelativePath))
		for _, key := range keys {
			if err := s.state.Reset(key); err != nil {
				return err
			}
		}
		return nil
	}
	downloadID, _, err := s.findOrigin(ctx, client, file)
	if err != nil {
		return fmt.Errorf("resolve origin before deletion: %w", err)
	}
	if expectedDownloadID != "" && !strings.EqualFold(downloadID, expectedDownloadID) {
		if downloadID == "" {
			return deferProcessing(errors.New("webhook origin is not yet visible in history"))
		}
		return errors.New("webhook origin does not match current file")
	}
	origin, err := s.prepareOrigin(ctx, client, file, downloadID)
	if err != nil {
		return fmt.Errorf("origin safety preflight: %w", err)
	}
	s.log.Warn("subtitle validation failed", "arr", client.Kind(), "file_id", file.ID, "reason", validation.Reason, "dry_run", s.config.DryRun, "arr_automatic_search", origin.automaticSearch)
	if s.config.MaxAttempts < 1 {
		return errors.New("invalid maximum attempts")
	}
	// Check both the current Arr resource and the exact local file after all
	// potentially slow history/queue reads, immediately before the write boundary.
	fresh, err = client.GetMediaFile(ctx, file.ID)
	if err != nil {
		return deferMissingMedia(err)
	}
	if !sameMedia(file, fresh, client.Kind()) {
		return deferProcessing(errors.New("media changed during preflight"))
	}
	info, err := os.Stat(pathOnDisk)
	if err != nil || !sameDiskFile(validation.fileInfo, info) {
		return deferProcessing(errors.New("local media changed or has no verified probe snapshot"))
	}
	if err := validation.checkSubtitleSnapshot(pathOnDisk); err != nil {
		return err
	}
	if client.Kind() == "sonarr" {
		currentIDs, err := client.EpisodeIDsForFile(ctx, file.ParentID, file.ID)
		if err != nil {
			return err
		}
		if !slices.Equal(ids, canonicalIDs(currentIDs)) {
			return deferProcessing(errors.New("episode mapping changed during preflight"))
		}
	}
	if s.config.DryRun {
		return nil
	}
	key := operationKey(client.Kind(), file.ID)
	op := Operation{Kind: client.Kind(), SubjectID: file.SubjectID(client.Kind()), FileID: file.ID, DownloadID: downloadID, EpisodeIDs: ids, Phase: "delete-requested", AutomaticSearch: origin.automaticSearch}
	attempt, started, err := s.state.Begin(key, op, keys)
	if err != nil || !started {
		return err
	}
	// Any error after Begin leaves a durable operation. Retry reads/jobs must not
	// mistake an underlying timeout for authorization to repeat the mutation.
	return s.finishMediaRemediation(ctx, client, file, origin, validation.Reason, key, ids, attempt)
}

func (s *Service) finishMediaRemediation(ctx context.Context, client *ArrClient, file MediaFile, origin originAction, reason, key string, ids []int, attempt int) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = requireReconciliation(resultErr)
		}
	}()
	if err := client.DeleteMediaFile(ctx, file.ID); err != nil {
		return fmt.Errorf("delete outcome requires reconciliation: %w", err)
	}
	if err := s.state.Phase(key, "deleted"); err != nil {
		return err
	}
	// Always finish origin remediation. Arr's automatic recovery is independent
	// of the guard's search budget and must never be followed by a duplicate search.
	if origin.queueID > 0 || origin.historyID > 0 {
		if origin.historyID > 0 {
			// Settings may have changed during deletion. Use the latest decision
			// and persist it before history failure can trigger a search.
			automatic, err := client.AutomaticHistorySearch(ctx, origin.releaseSource)
			if err != nil {
				return fmt.Errorf("refresh origin search policy after deletion: %w", err)
			}
			origin.automaticSearch = automatic
		}
		if err := s.state.OriginRequested(key, origin.automaticSearch); err != nil {
			return err
		}
		if err := s.applyOrigin(ctx, client, origin, reason); err != nil {
			return fmt.Errorf("origin outcome requires reconciliation: %w", err)
		}
		if err := s.state.Phase(key, "origin-complete"); err != nil {
			return err
		}
	}
	if origin.automaticSearch {
		s.log.Info("replacement search delegated to Arr; skipping guard search", "arr", client.Kind(), "file_id", file.ID, "attempt", attempt, "max_attempts", s.config.MaxAttempts)
	} else if attempt <= s.config.MaxAttempts {
		if err := s.searchRemaining(ctx, client, key, file.SubjectID(client.Kind()), ids); err != nil {
			return err
		}
	}
	return s.state.Complete(key)
}

func isOlderThanTenYears(year int, now time.Time) bool {
	return year > 0 && now.Year()-year > 10
}

func shouldSkipSilentMedia(year int, now time.Time) bool {
	return year > 0 && now.Year()-year > silentMediaSkipAfterYears
}

func (s *Service) skipSilentMediaGuard(client *ArrClient, file MediaFile) bool {
	if !shouldSkipSilentMedia(file.Year, time.Now()) {
		return false
	}
	s.log.Info("subtitle guard skipped media older than 50 years", "arr", client.Kind(), "file_id", file.ID, "year", file.Year)
	return true
}

func applyOldMediaGrace(validation Validation, year int, now time.Time) Validation {
	if !validation.Valid && validation.HasSubtitles && validation.HasUnknownLanguage && isOlderThanTenYears(year, now) {
		validation.Valid = true
		validation.Reason = "unidentified subtitle language accepted for media older than 10 years"
	}
	return validation
}

func retryKey(kind string, file MediaFile, searchEpisodeIDs []int, relativePath string) string {
	if kind == "sonarr" && len(searchEpisodeIDs) > 0 {
		ids := append([]int(nil), searchEpisodeIDs...)
		sort.Ints(ids)
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = strconv.Itoa(id)
		}
		return fmt.Sprintf("%s:episodes:%s", kind, strings.Join(parts, ","))
	}
	if kind == "radarr" && file.SubjectID(kind) > 0 {
		return fmt.Sprintf("%s:movie:%d", kind, file.SubjectID(kind))
	}
	return fmt.Sprintf("%s:%d:%s", kind, file.SubjectID(kind), strings.ToLower(filepath.Clean(relativePath)))
}

func (s *Service) mapPath(value string) string {
	original := value
	value = normalizePath(value)
	bestFrom := ""
	bestTo := ""
	bestOriginalTo := ""
	for _, mapping := range s.config.PathMappings {
		from := normalizePath(mapping.From)
		to := normalizePath(mapping.To)
		comparableValue, comparableFrom := comparableArrPath(value, mapping.From)
		prefix := strings.TrimRight(comparableFrom, "/") + "/"
		if comparableValue == comparableFrom || strings.HasPrefix(comparableValue, prefix) {
			if len(from) > len(bestFrom) {
				bestFrom = from
				bestTo = to
				bestOriginalTo = mapping.To
			}
		}
	}
	if bestFrom != "" {
		return mappedPath(bestTo, mappedSuffix(value, bestFrom), bestOriginalTo)
	}
	return original
}

func normalizePath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" {
		return ""
	}
	clean := pathpkg.Clean(value)
	if strings.HasPrefix(value, "//") {
		clean = "/" + clean
	}
	return clean
}

func mappedPath(to, suffix, originalTo string) string {
	result := pathpkg.Join(to, suffix)
	if strings.HasPrefix(to, "//") {
		result = "/" + result
	}
	if strings.Contains(originalTo, "\\") {
		result = strings.ReplaceAll(result, "/", "\\")
	}
	return result
}

func (s *Service) WebhookHandler(kind string) http.HandlerFunc {
	client := s.arr[kind]
	return func(w http.ResponseWriter, r *http.Request) {
		if client == nil {
			http.NotFound(w, r)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload WebhookPayload
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		if err := decoder.Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if !strings.EqualFold(payload.EventType, "download") && !strings.EqualFold(payload.EventType, "importcomplete") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, _, err := storedWebhook(kind, payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Enqueue(client, payload); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Service) authorized(r *http.Request) bool {
	tokenConfigured := s.config.WebhookToken != ""
	basicConfigured := s.config.WebhookUsername != "" || s.config.WebhookPassword != ""
	if !tokenConfigured && !basicConfigured {
		return true
	}
	if tokenConfigured {
		value := strings.TrimSpace(r.Header.Get("X-Webhook-Token"))
		if value == "" {
			authorization := strings.TrimSpace(r.Header.Get("Authorization"))
			if len(authorization) >= len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
				value = strings.TrimSpace(authorization[len("Bearer "):])
			}
		}
		if subtle.ConstantTimeCompare([]byte(value), []byte(s.config.WebhookToken)) == 1 {
			return true
		}
	}
	if basicConfigured {
		username, password, ok := r.BasicAuth()
		if ok && subtle.ConstantTimeCompare([]byte(username), []byte(s.config.WebhookUsername)) == 1 && subtle.ConstantTimeCompare([]byte(password), []byte(s.config.WebhookPassword)) == 1 {
			return true
		}
	}
	return false
}

func (s *Service) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	return s.serveOnListener(ctx, listener)
}

func (s *Service) serveOnListener(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Bind successfully before workers are allowed to run any recovery work.
	s.StartWorkers(ctx)
	defer s.StopWorkers()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	for kind := range s.arr {
		mux.HandleFunc("/webhook/"+kind, s.WebhookHandler(kind))
	}
	server := &http.Server{Addr: s.config.ListenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	s.log.Info("listening", "addr", s.config.ListenAddr)
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
