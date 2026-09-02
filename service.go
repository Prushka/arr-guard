package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	config  Config
	log     *slog.Logger
	probe   Prober
	state   *StateStore
	arr     map[string]*ArrClient
	jobs    chan webhookJob
	stop    chan struct{}
	wg      sync.WaitGroup
	locks   sync.Map
	handled sync.Map
}

type webhookJob struct {
	client  *ArrClient
	payload WebhookPayload
}

func NewService(config Config, log *slog.Logger) (*Service, error) {
	state, err := LoadStateStore(config.StatePath)
	if err != nil {
		return nil, err
	}
	service := &Service{
		config: config,
		log:    log,
		probe:  Prober{Path: config.FFprobePath, Timeout: 10 * time.Minute},
		state:  state,
		arr:    make(map[string]*ArrClient),
		jobs:   make(chan webhookJob, config.Workers*4),
		stop:   make(chan struct{}),
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
}

func (s *Service) StopWorkers() {
	close(s.stop)
	s.wg.Wait()
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
	for _, webhookFile := range files {
		if webhookFile.ID == 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", client.Kind(), webhookFile.ID)
		if _, loaded := s.handled.LoadOrStore(key, struct{}{}); loaded {
			continue
		}
		mutex := s.fileMutex(key)
		mutex.Lock()
		file, err := client.GetMediaFile(ctx, webhookFile.ID)
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
			err = s.validateAndRemediateWithHistory(ctx, client, file, downloadID, importedHistoryID, episodeIDsForFile(payload, webhookFile.ID))
		}
		mutex.Unlock()
		if err != nil {
			s.handled.Delete(key)
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

func (s *Service) fileMutex(key string) *sync.Mutex {
	value, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
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
	validation, pathOnDisk, err := s.validate(ctx, file)
	if err != nil {
		return err
	}
	return s.applyValidation(ctx, client, file, validation, pathOnDisk, downloadID, importedHistoryID, searchEpisodeIDs)
}

func (s *Service) validate(ctx context.Context, file MediaFile) (Validation, string, error) {
	pathOnDisk := s.mapPath(file.Path)
	if file.ID < 1 {
		return Validation{}, pathOnDisk, errors.New("media file ID is missing")
	}
	if pathOnDisk == "" || pathOnDisk == "." {
		return Validation{}, pathOnDisk, fmt.Errorf("media file %d has no path", file.ID)
	}
	validation, err := s.probe.Validate(ctx, pathOnDisk)
	if err != nil {
		return Validation{}, pathOnDisk, fmt.Errorf("probe %s: %w", pathOnDisk, err)
	}
	validation = applyOldMediaGrace(validation, file.Year, time.Now())
	return validation, pathOnDisk, nil
}

func (s *Service) applyValidation(ctx context.Context, client *ArrClient, file MediaFile, validation Validation, pathOnDisk, downloadID string, importedHistoryID int, searchEpisodeIDs []int) error {
	if !validation.Valid && !s.config.DryRun && client.Kind() == "sonarr" && len(searchEpisodeIDs) == 0 {
		var err error
		searchEpisodeIDs, err = client.EpisodeIDsForFile(ctx, file.ParentID, file.ID)
		if err != nil {
			return fmt.Errorf("resolve Sonarr episodes for file %d: %w", file.ID, err)
		}
		if len(searchEpisodeIDs) == 0 {
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
	var remediationErrors []error
	if downloadID != "" {
		if err := s.failOrigin(ctx, client, downloadID, importedHistoryID, validation.Reason); err != nil {
			remediationErrors = append(remediationErrors, fmt.Errorf("fail originating download: %w", err))
		}
	}
	if err := client.SearchEpisodes(ctx, file.SubjectID(client.Kind()), searchEpisodeIDs); err != nil {
		remediationErrors = append(remediationErrors, fmt.Errorf("search %s subject %d: %w", client.Kind(), file.SubjectID(client.Kind()), err))
	}
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
