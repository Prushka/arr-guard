package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	config    Config
	log       *slog.Logger
	probe     Prober
	probeFn   func(context.Context, string) (Validation, error)
	state     *StateStore
	arr       map[string]*ArrClient
	jobs      chan webhookJob
	stop      chan struct{}
	wg        sync.WaitGroup
	locksMu   sync.Mutex
	locks     map[string]*fileLock
	handledMu sync.Mutex
	handled   map[string]time.Time
	stopOnce  sync.Once
	pendingMu sync.Mutex
	pending   map[string]*pendingRemediation
}

// fileLock tracks waiters so an idle lock can be removed without allowing
// another webhook to create a second lock for the same media file.
type fileLock struct {
	mu   sync.Mutex
	refs int
}

type webhookJob struct {
	client  *ArrClient
	payload WebhookPayload
}

// pendingRemediation records the post-delete work that must still be completed.
// It is intentionally kept in memory: this is a shutdown safety net for work
// already started by this process, not a second durable job queue.
type pendingRemediation struct {
	key               string
	client            *ArrClient
	subjectID         int
	downloadID        string
	importedHistoryID int
	reason            string
	searchEpisodeIDs  []int
	originPending     bool
	searchPending     bool
}

const blockedQueueScanInterval = time.Hour

// Keep successful webhook IDs briefly to absorb Arr retries while bounding
// memory use. Failed processing removes the claim immediately for retry.
const webhookDedupTTL = 24 * time.Hour

func NewService(config Config, log *slog.Logger) (*Service, error) {
	state, err := LoadStateStore(config.StatePath)
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
		pending: make(map[string]*pendingRemediation),
	}
	if config.Sonarr != nil {
		service.arr["sonarr"] = NewArrClient(*config.Sonarr, log)
	}
	if config.Radarr != nil {
		service.arr["radarr"] = NewArrClient(*config.Radarr, log)
	}
	return service, nil
}

func (s *Service) StartWorkers(ctx context.Context) {
	for i := 0; i < s.config.Workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case item := <-s.jobs:
					jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
					if err := s.processWebhook(jobCtx, item.client, item.payload); err != nil && !errors.Is(err, context.Canceled) {
						s.log.Error("webhook processing failed", "error", err, "arr", item.client.Kind())
					}
					cancel()
				case <-s.stop:
					return
				}
			}
		}()
	}
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
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// CleanupPending retries the post-delete blocklist/failure and replacement
// search operations that were interrupted by an API error or shutdown.
// Callers should stop workers first so no new pending entries are added while
// the cleanup pass is running.
func (s *Service) CleanupPending(ctx context.Context) error {
	pending := s.pendingSnapshot()
	var cleanupErrors []error
	for _, item := range pending {
		if item.originPending {
			if err := s.failOrigin(ctx, item.client, item.downloadID, item.importedHistoryID, item.reason); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup %s origin for %s: %w", item.client.Kind(), item.key, err))
			} else {
				s.markPendingOriginDone(item.key)
			}
		}
		if item.searchPending {
			if err := item.client.SearchEpisodes(ctx, item.subjectID, item.searchEpisodeIDs); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup %s search for %s: %w", item.client.Kind(), item.key, err))
			} else {
				s.markPendingSearchDone(item.key)
			}
		}
		s.removePendingIfComplete(item.key)
	}
	return errors.Join(cleanupErrors...)
}

func (s *Service) pendingSnapshot() []*pendingRemediation {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	items := make([]*pendingRemediation, 0, len(s.pending))
	for _, item := range s.pending {
		copy := *item
		copy.searchEpisodeIDs = append([]int(nil), item.searchEpisodeIDs...)
		items = append(items, &copy)
	}
	return items
}

func (s *Service) registerPending(item pendingRemediation) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]*pendingRemediation)
	}
	s.pending[item.key] = &item
}

func (s *Service) markPendingOriginDone(key string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if item := s.pending[key]; item != nil {
		item.originPending = false
	}
}

func (s *Service) markPendingSearchDone(key string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if item := s.pending[key]; item != nil {
		item.searchPending = false
	}
}

func (s *Service) removePendingIfComplete(key string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if item := s.pending[key]; item != nil && !item.originPending && !item.searchPending {
		delete(s.pending, key)
	}
}

func (s *Service) runBlockedQueueScan(ctx context.Context) {
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
	for _, item := range queue {
		if !item.needsImportRecovery() {
			continue
		}
		if err := s.recoverBlockedQueueItem(ctx, client, item); err != nil {
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

func (s *Service) recoverBlockedQueueItem(ctx context.Context, client *ArrClient, item QueueRecord) error {
	if item.ID < 1 {
		return errors.New("queue item ID is missing")
	}
	if client.Kind() == "radarr" {
		if item.MovieID < 1 {
			return errors.New("Radarr queue item movie ID is missing")
		}
	} else if item.EpisodeID < 1 {
		return errors.New("Sonarr queue item episode ID is missing")
	}

	s.log.Warn("blocked queue item requires regrab", "arr", client.Kind(), "queue_id", item.ID, "download_id", item.DownloadID, "movie_id", item.MovieID, "series_id", item.SeriesID, "episode_id", item.EpisodeID, "dry_run", s.config.DryRun)
	if s.config.DryRun {
		return nil
	}
	if err := client.FailQueueItem(ctx, item.ID, "Subtitle Guard: unable to import automatically"); err != nil {
		stillQueued, verifyErr := queueItemPresent(ctx, client, item.ID)
		if verifyErr != nil {
			return fmt.Errorf("remove and blocklist queue item: %w (verification failed: %v)", err, verifyErr)
		}
		if stillQueued {
			return fmt.Errorf("remove and blocklist queue item: %w", err)
		}
		// Arr may finish removing the download but time out its HTTP response
		// while the download client is being updated. If the queue item is gone,
		// its blocklist/failure operation completed and it is safe to search.
		s.log.Warn("queue failure request timed out after item disappeared; continuing replacement search", "arr", client.Kind(), "queue_id", item.ID)
	}
	if client.Kind() == "radarr" {
		if err := client.SearchEpisodes(ctx, item.MovieID, nil); err != nil {
			return fmt.Errorf("search movie %d: %w", item.MovieID, err)
		}
		return nil
	}
	if err := client.SearchEpisodes(ctx, item.SeriesID, []int{item.EpisodeID}); err != nil {
		return fmt.Errorf("search episode %d: %w", item.EpisodeID, err)
	}
	return nil
}

func queueItemPresent(ctx context.Context, client *ArrClient, id int) (bool, error) {
	queue, err := client.Queue(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range queue {
		if item.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) Enqueue(client *ArrClient, payload WebhookPayload) error {
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
	seen := make(map[string]struct{}, len(files))
	for _, webhookFile := range files {
		if webhookFile.ID == 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", client.Kind(), webhookFile.ID)
		if _, alreadySeen := seen[key]; alreadySeen {
			continue
		}
		seen[key] = struct{}{}
		if !s.claimWebhook(key) {
			continue
		}
		lock := s.acquireFileLock(key)
		var err error
		func() {
			defer s.releaseFileLock(key, lock)
			defer func() { s.finishWebhook(key, err) }()
			file, getErr := client.GetMediaFile(ctx, webhookFile.ID)
			err = getErr
			if err == nil {
				if file.Path == "" {
					file.Path = webhookFile.Path
				}
				if file.RelativePath == "" {
					file.RelativePath = webhookFile.RelativePath
				}
				if file.Path == "" {
					if client.Kind() == "sonarr" && payload.Series != nil {
						file.Path = joinArrPath(payload.Series.Path, file.RelativePath)
					} else if client.Kind() == "radarr" && payload.Movie != nil {
						parentPath := payload.Movie.Path
						if parentPath == "" {
							parentPath = payload.Movie.FolderPath
						}
						file.Path = joinArrPath(parentPath, file.RelativePath)
					}
				}
				if client.Kind() == "sonarr" && file.ParentID == 0 && payload.Series != nil {
					file.ParentID = payload.Series.ID
				}
				if client.Kind() == "sonarr" && file.Year == 0 && payload.Series != nil {
					file.Year = payload.Series.Year
				}
				if client.Kind() == "radarr" && file.MovieID == 0 && payload.Movie != nil {
					file.MovieID = payload.Movie.ID
				}
				if client.Kind() == "radarr" && file.Year == 0 && payload.Movie != nil {
					file.Year = payload.Movie.Year
				}
				downloadID := payload.DownloadID
				var importedHistoryID int
				if downloadID == "" {
					var historyErr error
					downloadID, importedHistoryID, historyErr = s.findOrigin(ctx, client, file)
					if historyErr != nil {
						s.log.Warn("could not resolve file history", "arr", client.Kind(), "file_id", file.ID, "error", historyErr)
					}
				}
				err = s.validateAndRemediateWithHistoryOptions(ctx, client, file, downloadID, importedHistoryID, episodeIDsForFile(payload, webhookFile.ID), true)
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
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
	for _, client := range s.arr {
		files, err := client.ListLibraryFiles(ctx)
		if err != nil {
			return fmt.Errorf("audit %s: %w", client.Kind(), err)
		}
		s.log.Info("library scan started", "arr", client.Kind(), "files", len(files))
		sem := make(chan struct{}, s.config.Workers)
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex
		for _, mediaFile := range files {
			file := mediaFile
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				if err := s.auditFile(ctx, client, file); err != nil {
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
			return fmt.Errorf("audit %s: %w", client.Kind(), firstErr)
		}
		s.log.Info("library scan complete", "arr", client.Kind())
	}
	return nil
}

// ScanUnmatched lists media files beneath the configured mapped library roots
// that do not have a matching Sonarr or Radarr media-file ID. It intentionally
// does not probe or call applyValidation: an orphan has no Arr media-file ID,
// so subtitle remediation is neither possible nor safe.
func (s *Service) ScanUnmatched(ctx context.Context) error {
	roots := scanRoots(s.config.PathMappings)
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
					matched[scanPathKey(path)] = struct{}{}
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
	if err := writeUnmatchedReport(s.config.InvalidPath, report); err != nil {
		return err
	}
	s.log.Info("unmatched scan complete", "roots", len(roots), "media_files", scanned, "unmatched_files", len(report.Files), "output", s.config.InvalidPath)
	return nil
}

func (s *Service) isUnmatchedExcludedDir(root, path string) bool {
	pathKey := scanPathKey(path)
	for _, excluded := range s.config.UnmatchedExcludeDirs {
		excluded = strings.TrimSpace(excluded)
		if excluded == "" {
			continue
		}
		excludedKey := scanPathKey(filepath.Clean(excluded))
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
		return errors.New("INVALID_PATH must not be empty")
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
	validation, pathOnDisk, err := s.validate(ctx, file)
	if err != nil {
		return err
	}
	var downloadID string
	var historyID int
	if !validation.Valid && !s.config.DryRun {
		downloadID, historyID, err = s.findOrigin(ctx, client, file)
		if err != nil {
			s.log.Warn("could not resolve file history", "arr", client.Kind(), "file_id", file.ID, "error", err)
		}
	}
	return s.applyValidation(ctx, client, file, validation, pathOnDisk, downloadID, historyID, nil)
}

func (s *Service) findOrigin(ctx context.Context, client *ArrClient, file MediaFile) (string, int, error) {
	records, err := client.SubjectHistory(ctx, file.SubjectID(client.Kind()))
	if err != nil {
		return "", 0, err
	}
	for _, record := range records {
		if strings.EqualFold(record.EventType, "downloadFolderImported") && historyData(record, "fileId") == strconv.Itoa(file.ID) {
			return record.DownloadID, record.ID, nil
		}
	}
	return "", 0, nil
}

func historyData(record HistoryRecord, key string) string {
	for recordKey, value := range record.Data {
		if strings.EqualFold(recordKey, key) {
			return value
		}
	}
	return ""
}

func (s *Service) validateAndRemediate(ctx context.Context, client *ArrClient, file MediaFile, downloadID string) error {
	return s.validateAndRemediateWithHistory(ctx, client, file, downloadID, 0, nil)
}

func (s *Service) validateAndRemediateWithHistory(ctx context.Context, client *ArrClient, file MediaFile, downloadID string, importedHistoryID int, searchEpisodeIDs []int) error {
	return s.validateAndRemediateWithHistoryOptions(ctx, client, file, downloadID, importedHistoryID, searchEpisodeIDs, false)
}

func (s *Service) validateAndRemediateWithHistoryOptions(ctx context.Context, client *ArrClient, file MediaFile, downloadID string, importedHistoryID int, searchEpisodeIDs []int, resolveEpisodesForValid bool) error {
	validation, pathOnDisk, err := s.validate(ctx, file)
	if err != nil {
		return err
	}
	return s.applyValidationOptions(ctx, client, file, validation, pathOnDisk, downloadID, importedHistoryID, searchEpisodeIDs, resolveEpisodesForValid)
}

func (s *Service) validate(ctx context.Context, file MediaFile) (Validation, string, error) {
	pathOnDisk := s.mapPath(file.Path)
	if file.ID < 1 {
		return Validation{}, pathOnDisk, errors.New("media file ID is missing")
	}
	if pathOnDisk == "" || pathOnDisk == "." {
		return Validation{}, pathOnDisk, fmt.Errorf("media file %d has no path", file.ID)
	}
	validation, err := s.probePath(ctx, pathOnDisk)
	if err != nil {
		return Validation{}, pathOnDisk, fmt.Errorf("probe %s: %w", pathOnDisk, err)
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

func (s *Service) applyValidation(ctx context.Context, client *ArrClient, file MediaFile, validation Validation, pathOnDisk, downloadID string, importedHistoryID int, searchEpisodeIDs []int) error {
	return s.applyValidationOptions(ctx, client, file, validation, pathOnDisk, downloadID, importedHistoryID, searchEpisodeIDs, false)
}

func (s *Service) applyValidationOptions(ctx context.Context, client *ArrClient, file MediaFile, validation Validation, pathOnDisk, downloadID string, importedHistoryID int, searchEpisodeIDs []int, resolveEpisodesForValid bool) error {
	if !s.config.DryRun && client.Kind() == "sonarr" && len(searchEpisodeIDs) == 0 && (!validation.Valid || resolveEpisodesForValid) {
		var err error
		searchEpisodeIDs, err = client.EpisodeIDsForFile(ctx, file.ParentID, file.ID)
		if err != nil {
			if !validation.Valid {
				return fmt.Errorf("resolve Sonarr episodes for file %d: %w", file.ID, err)
			}
			s.log.Warn("could not resolve Sonarr episodes for valid file; resetting path retry state only", "file_id", file.ID, "error", err)
		}
		if !validation.Valid && len(searchEpisodeIDs) == 0 {
			return fmt.Errorf("Sonarr file %d is not assigned to an episode; refusing broad series search", file.ID)
		}
	}
	relativePath := file.RelativePath
	if relativePath == "" {
		relativePath = file.Path
	}
	key := retryKey(client.Kind(), file, searchEpisodeIDs, relativePath)
	if validation.Valid {
		if !s.config.DryRun {
			if err := s.state.Reset(key); err != nil {
				s.log.Warn("could not reset retry state", "error", err, "key", key)
			}
			// Older versions used a path key when episode metadata was absent.
			// Reset it as well so a successful replacement clears legacy state.
			legacyKey := retryKey(client.Kind(), file, nil, relativePath)
			if legacyKey != key {
				if err := s.state.Reset(legacyKey); err != nil {
					s.log.Warn("could not reset legacy retry state", "error", err, "key", legacyKey)
				}
			}
		}
		return nil
	}
	attempt := s.state.Attempts(key) + 1
	s.log.Warn("subtitle validation failed", "arr", client.Kind(), "file", pathOnDisk, "reason", validation.Reason, "attempt", attempt, "max_attempts", s.config.MaxAttempts)
	if s.config.DryRun {
		return nil
	}
	if file.SubjectID(client.Kind()) < 1 {
		return fmt.Errorf("refusing to delete media file %d: parent ID is missing", file.ID)
	}
	if client.Kind() == "sonarr" && len(searchEpisodeIDs) == 0 {
		return fmt.Errorf("refusing to delete Sonarr file %d without affected episode IDs", file.ID)
	}
	attempt, err := s.state.Increment(key)
	if err != nil {
		return err
	}
	if err := client.DeleteMediaFile(ctx, file.ID); err != nil {
		return fmt.Errorf("delete %s media file %d: %w", client.Kind(), file.ID, err)
	}
	if attempt > s.config.MaxAttempts {
		s.log.Error("maximum replacement attempts reached; invalid file deleted without another search", "arr", client.Kind(), "file", pathOnDisk)
		return nil
	}
	pending := pendingRemediation{
		key:               fmt.Sprintf("%s:file:%d", client.Kind(), file.ID),
		client:            client,
		subjectID:         file.SubjectID(client.Kind()),
		downloadID:        downloadID,
		importedHistoryID: importedHistoryID,
		reason:            validation.Reason,
		searchEpisodeIDs:  append([]int(nil), searchEpisodeIDs...),
		originPending:     downloadID != "",
		searchPending:     true,
	}
	s.registerPending(pending)
	var remediationErrors []error
	if pending.originPending {
		if err := s.failOrigin(ctx, client, downloadID, importedHistoryID, validation.Reason); err != nil {
			remediationErrors = append(remediationErrors, fmt.Errorf("fail originating download: %w", err))
		} else {
			s.markPendingOriginDone(pending.key)
		}
	} else {
		// A manually managed/old file may have no Arr import history. It is
		// still removed and searched, but there is no release identity to block.
		s.log.Info("no originating import history; skipping blocklist", "arr", client.Kind(), "file_id", file.ID)
	}
	if err := client.SearchEpisodes(ctx, file.SubjectID(client.Kind()), searchEpisodeIDs); err != nil {
		remediationErrors = append(remediationErrors, fmt.Errorf("search %s subject %d: %w", client.Kind(), file.SubjectID(client.Kind()), err))
	} else {
		s.markPendingSearchDone(pending.key)
	}
	s.removePendingIfComplete(pending.key)
	return errors.Join(remediationErrors...)
}

func episodeIDs(payload WebhookPayload) []int {
	ids := make([]int, 0, len(payload.Episodes))
	for _, episode := range payload.Episodes {
		if episode.ID > 0 {
			ids = append(ids, episode.ID)
		}
	}
	return ids
}

func episodeIDsForFile(payload WebhookPayload, fileID int) []int {
	if fileID > 0 {
		mapped := make([]int, 0)
		for _, episode := range payload.Episodes {
			if episode.ID > 0 && episode.EpisodeFileID == fileID {
				mapped = append(mapped, episode.ID)
			}
		}
		if len(mapped) > 0 {
			return mapped
		}
	}
	if len(payload.files("sonarr")) == 1 {
		return episodeIDs(payload)
	}
	return nil
}

func isOlderThanTenYears(year int, now time.Time) bool {
	return year > 0 && now.Year()-year > 10
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

func (s *Service) failOrigin(ctx context.Context, client *ArrClient, downloadID string, importedHistoryID int, reason string) error {
	queue, err := client.Queue(ctx)
	if err != nil {
		return err
	}
	for _, item := range queue {
		if strings.EqualFold(item.DownloadID, downloadID) {
			if err := client.FailQueueItem(ctx, item.ID, "Subtitle Guard: "+reason); err != nil {
				return fmt.Errorf("blocklist queue item %d: %w", item.ID, err)
			}
			return nil
		}
	}
	if importedHistoryID != 0 {
		return client.MarkHistoryFailed(ctx, importedHistoryID)
	}
	records, err := client.DownloadHistory(ctx, downloadID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if strings.EqualFold(record.EventType, "grabbed") {
			return client.MarkHistoryFailed(ctx, record.ID)
		}
	}
	return nil
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
		prefix := strings.TrimRight(from, "/") + "/"
		if value == from || strings.HasPrefix(value, prefix) {
			if len(from) > len(bestFrom) {
				bestFrom = from
				bestTo = to
				bestOriginalTo = mapping.To
			}
		}
	}
	if bestFrom != "" {
		return mappedPath(bestTo, strings.TrimPrefix(value, strings.TrimRight(bestFrom, "/")), bestOriginalTo)
	}
	return original
}

func normalizePath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" {
		return ""
	}
	return pathpkg.Clean(value)
}

func mappedPath(to, suffix, originalTo string) string {
	result := pathpkg.Join(to, suffix)
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
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload WebhookPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
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
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
