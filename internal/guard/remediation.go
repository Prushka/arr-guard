package guard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/pathutil"
	"github.com/Prushka/arr-guard/internal/probe"
)

func (s *Service) auditFile(ctx context.Context, client *arr.Client, file arr.MediaFile) error {
	return s.auditFileWithOrigin(ctx, client, file, "")
}

func (s *Service) auditFileWithOrigin(ctx context.Context, client *arr.Client, file arr.MediaFile, expectedDownloadID string) error {
	if s.skipSilentMediaGuard(client, file) {
		return nil
	}
	validation, pathOnDisk, err := s.validate(ctx, file)
	if err != nil {
		return err
	}
	return s.applyValidationWithOrigin(ctx, client, file, validation, pathOnDisk, expectedDownloadID)
}

func (s *Service) findOrigin(ctx context.Context, client *arr.Client, file arr.MediaFile) (string, int, error) {
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

func historyData(record arr.HistoryRecord, key string) string {
	for recordKey, value := range record.Data {
		if strings.EqualFold(recordKey, key) {
			return value
		}
	}
	return ""
}

func (s *Service) validate(ctx context.Context, file arr.MediaFile) (probe.Validation, string, error) {
	pathOnDisk := s.mapPath(file.Path)
	if file.Path != strings.TrimSpace(file.Path) {
		return probe.Validation{}, pathOnDisk, errors.New("ambiguous whitespace in media path")
	}
	if file.ID < 1 {
		return probe.Validation{}, pathOnDisk, errors.New("media file ID is missing")
	}
	if pathOnDisk == "" || pathOnDisk == "." {
		return probe.Validation{}, pathOnDisk, fmt.Errorf("media file %d has no path", file.ID)
	}
	if len(s.config.PathMappings) > 0 {
		matched := false
		for _, mapping := range s.config.PathMappings {
			value, from := pathutil.ComparableArr(file.Path, mapping.From)
			if value == from || strings.HasPrefix(value, strings.TrimRight(from, "/")+"/") {
				matched = true
				break
			}
		}
		if !matched {
			return probe.Validation{}, pathOnDisk, errors.New("media path is outside configured mappings")
		}
	}
	validation, err := s.probePath(ctx, pathOnDisk)
	if err != nil {
		return probe.Validation{}, pathOnDisk, deferProcessing(fmt.Errorf("probe %s: %w", pathOnDisk, err))
	}
	if file.Size > 0 && validation.FileInfo != nil && validation.FileInfo.Size() != file.Size {
		return probe.Validation{}, pathOnDisk, deferProcessing(errors.New("local file size does not match Arr metadata"))
	}
	if len(validation.ProbeWarnings) > 0 {
		s.log.Warn("ffprobe recovered from video diagnostic; applying subtitle policy", "file_id", file.ID, "diagnostics", validation.ProbeWarnings)
	}
	validation = applyOldMediaGrace(validation, file.Year, time.Now())
	return validation, pathOnDisk, nil
}

func (s *Service) probePath(ctx context.Context, path string) (probe.Validation, error) {
	if s.probeFn != nil {
		return s.probeFn(ctx, path)
	}
	return s.probe.Validate(ctx, path)
}

func (s *Service) applyValidation(ctx context.Context, client *arr.Client, file arr.MediaFile, validation probe.Validation, pathOnDisk string) error {
	return s.applyValidationWithOrigin(ctx, client, file, validation, pathOnDisk, "")
}

func (s *Service) applyValidationWithOrigin(ctx context.Context, client *arr.Client, file arr.MediaFile, validation probe.Validation, pathOnDisk, expectedDownloadID string) error {
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
		ids = arr.CanonicalIDs(ids)
		if len(ids) == 0 {
			return deferProcessing(errors.New("sonarr file is not assigned to any episode"))
		}
	}
	keys := retryKeys(client.Kind(), file, ids)
	if validation.Valid {
		info, err := os.Stat(pathOnDisk)
		if err != nil || !pathutil.SameFile(validation.FileInfo, info) {
			return deferProcessing(errors.New("valid media changed or has no verified snapshot before retry-state reset"))
		}
		if err := validation.CheckSubtitleSnapshot(pathOnDisk); err != nil {
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
	if err != nil || !pathutil.SameFile(validation.FileInfo, info) {
		return deferProcessing(errors.New("local media changed or has no verified probe snapshot"))
	}
	if err := validation.CheckSubtitleSnapshot(pathOnDisk); err != nil {
		return err
	}
	if client.Kind() == "sonarr" {
		currentIDs, err := client.EpisodeIDsForFile(ctx, file.ParentID, file.ID)
		if err != nil {
			return err
		}
		if !slices.Equal(ids, arr.CanonicalIDs(currentIDs)) {
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

func (s *Service) finishMediaRemediation(ctx context.Context, client *arr.Client, file arr.MediaFile, origin originAction, reason, key string, ids []int, attempt int) (resultErr error) {
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

func (s *Service) skipSilentMediaGuard(client *arr.Client, file arr.MediaFile) bool {
	if !shouldSkipSilentMedia(file.Year, time.Now()) {
		return false
	}
	s.log.Info("subtitle guard skipped media older than 50 years", "arr", client.Kind(), "file_id", file.ID, "year", file.Year)
	return true
}

func applyOldMediaGrace(validation probe.Validation, year int, now time.Time) probe.Validation {
	if !validation.Valid && validation.HasSubtitles && validation.HasUnknownLanguage && isOlderThanTenYears(year, now) {
		validation.Valid = true
		validation.Reason = "unidentified subtitle language accepted for media older than 10 years"
	}
	return validation
}

func retryKey(kind string, file arr.MediaFile, searchEpisodeIDs []int, relativePath string) string {
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
	value = pathutil.Normalize(value)
	bestFrom := ""
	bestTo := ""
	bestOriginalTo := ""
	for _, mapping := range s.config.PathMappings {
		from := pathutil.Normalize(mapping.From)
		to := pathutil.Normalize(mapping.To)
		comparableValue, comparableFrom := pathutil.ComparableArr(value, mapping.From)
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
		return pathutil.Mapped(bestTo, pathutil.MappedSuffix(value, bestFrom), bestOriginalTo)
	}
	return original
}
