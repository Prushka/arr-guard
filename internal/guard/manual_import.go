package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/pathutil"
	"github.com/Prushka/arr-guard/internal/probe"
)

type checkedImport struct {
	managed    arr.MediaFile
	request    arr.ManualImportFile
	expect     ImportExpectation
	validation probe.Validation
	localPath  string
}

func arrPathWithin(root, target string) bool {
	target, root = pathutil.ComparableArr(target, root)
	return root != "" && root != "." && (target == root || strings.HasPrefix(target, strings.TrimRight(root, "/")+"/"))
}

func (s *Service) checkImportQueue(ctx context.Context, c *arr.Client, item arr.QueueRecord, plan queueRecoveryPlan, requireIDReason bool) (string, error) {
	queue, err := c.Queue(ctx)
	if err != nil {
		return "", err
	}
	subject, rows, err := queueRecoveryGroup(c.Kind(), item, queue, plan.subjectID)
	if err != nil {
		return "", err
	}
	if subject != plan.subjectID || !slices.Equal(rows, plan.rows) {
		return "", deferProcessing(errors.New("manual import queue group changed"))
	}
	root := ""
	for _, q := range queue {
		if !strings.EqualFold(q.DownloadID, item.DownloadID) {
			continue
		}
		if requireIDReason && !strings.EqualFold(q.TrackedDownloadState, "imported") && !q.AllowsMatchedIDImport(c.Kind()) {
			return "", errors.New("manual import requires only the matched-by-ID waiting reason")
		}
		if q.OutputPath == "" || q.OutputPath != strings.TrimSpace(q.OutputPath) {
			return "", errors.New("manual import download output path is missing or ambiguous")
		}
		if root != "" && !sameArrPath(root, q.OutputPath) {
			return "", errors.New("manual import queue output paths conflict")
		}
		root = q.OutputPath
	}
	// Arr may move files and clean up a download during ManualImport itself.
	// Protect consumers in every configured instance before submitting it.
	for _, other := range s.arr {
		if other == c {
			continue
		}
		otherQueue, err := other.Queue(ctx)
		if err != nil {
			return "", fmt.Errorf("manual import shared-download check: %w", err)
		}
		for _, q := range otherQueue {
			if strings.EqualFold(q.DownloadID, item.DownloadID) || s.importPathsOverlap(root, q.OutputPath) {
				return "", errors.New("manual import download is shared with another configured Arr instance")
			}
		}
	}
	return root, nil
}

func (s *Service) importPathsOverlap(a, b string) bool {
	if b == "" {
		return false
	}
	if arrPathWithin(a, b) || arrPathWithin(b, a) {
		return true
	}
	a, b = s.mapPath(a), s.mapPath(b)
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	a, b = pathutil.ResolveExisting(a), pathutil.ResolveExisting(b)
	return pathutil.Within(a, b) || pathutil.Within(b, a)
}

func sameArrPath(a, b string) bool {
	a, b = pathutil.ComparableArr(a, b)
	return a == b
}

func importEpisodeIDs(episodes []arr.Episode, subject int) ([]int, error) {
	var ids []int
	for _, e := range episodes {
		if e.ID < 1 || e.SeriesID != subject {
			return nil, errors.New("manual import episode identity is invalid")
		}
		ids = append(ids, e.ID)
	}
	canonical := arr.CanonicalIDs(ids)
	if len(canonical) != len(ids) {
		return nil, errors.New("manual import episode identity is duplicated")
	}
	return canonical, nil
}

func (s *Service) checkImportCandidate(ctx context.Context, c *arr.Client, item arr.QueueRecord, plan queueRecoveryPlan, root string, candidate arr.ImportCandidate) (checkedImport, error) {
	checked := checkedImport{}
	if candidate.ID < 1 || candidate.Size < 1 || !strings.EqualFold(candidate.DownloadID, item.DownloadID) || candidate.Path != strings.TrimSpace(candidate.Path) || !pathutil.IsMedia(candidate.Path) || !arrPathWithin(root, candidate.Path) {
		return checked, errors.New("manual import candidate has invalid identity, size, or source path")
	}
	if candidate.MovieFileID != 0 || candidate.EpisodeFileID != 0 {
		return checked, errors.New("manual import candidate is already a library file")
	}
	if candidate.Rejections == nil {
		return checked, errors.New("manual import candidate rejection inventory is missing")
	}
	for _, rejection := range candidate.Rejections {
		if !arr.MatchedIDImportReason(c.Kind(), rejection.Reason) {
			return checked, fmt.Errorf("manual import candidate has an additional rejection: %s", rejection.Reason)
		}
	}
	var quality struct {
		Quality struct {
			ID int `json:"id"`
		} `json:"quality"`
	}
	if json.Unmarshal(candidate.Quality, &quality) != nil || quality.Quality.ID < 1 {
		return checked, errors.New("manual import candidate quality is unknown")
	}
	var languages []struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(candidate.Languages, &languages) != nil || len(languages) == 0 {
		return checked, errors.New("manual import candidate language metadata is missing")
	}
	// Filename parsing is deliberately independent of download history. Passing
	// an intended subject ID or synthesizing a title here would defeat this check.
	parsed, err := c.ParseImportName(ctx, path.Base(pathutil.Normalize(candidate.Path)))
	if err != nil {
		return checked, fmt.Errorf("manual import filename identity: %w", err)
	}
	file := arr.MediaFile{ID: candidate.ID, Path: candidate.Path, Size: candidate.Size}
	libraryPath := ""
	request := arr.ManualImportFile{Path: candidate.Path, FolderName: candidate.FolderName, Quality: candidate.Quality, Languages: candidate.Languages, ReleaseGroup: candidate.ReleaseGroup, IndexerFlags: candidate.IndexerFlags, ReleaseType: candidate.ReleaseType, DownloadID: item.DownloadID}
	if c.Kind() == "radarr" {
		if candidate.Movie == nil || candidate.Movie.ID != plan.subjectID {
			return checked, errors.New("manual import filename does not independently identify the intended movie")
		}
		if err := s.resolveImportMovie(ctx, c, parsed, plan.subjectID); err != nil {
			return checked, err
		}
		file.MovieID, file.Year, request.MovieID = plan.subjectID, candidate.Movie.Year, plan.subjectID
		libraryPath = candidate.Movie.Path
		if arrPathWithin(candidate.Movie.Path, candidate.Path) {
			return checked, errors.New("manual import source is inside the library")
		}
	} else {
		if candidate.Series == nil || candidate.Series.ID != plan.subjectID {
			return checked, errors.New("manual import filename does not independently identify the intended series")
		}
		ids, err := importEpisodeIDs(candidate.Episodes, plan.subjectID)
		if err != nil {
			return checked, err
		}
		parsedIDs, err := s.resolveImportEpisodes(ctx, c, parsed, plan.subjectID)
		if err != nil {
			return checked, err
		}
		if len(ids) == 0 || !slices.Equal(ids, parsedIDs) {
			return checked, errors.New("manual import filename episode mapping is ambiguous or conflicting")
		}
		for _, id := range ids {
			if !slices.Contains(plan.searchIDs, id) {
				return checked, errors.New("manual import would affect an existing or unreserved episode")
			}
		}
		file.ParentID, request.SeriesID, request.EpisodeIDs = plan.subjectID, plan.subjectID, ids
		libraryPath = candidate.Series.Path
		for _, e := range candidate.Episodes {
			file.Year = max(file.Year, e.ReleaseYear())
		}
		if arrPathWithin(candidate.Series.Path, candidate.Path) {
			return checked, errors.New("manual import source is inside the library")
		}
	}
	// Resolve both sides so a symlink cannot redirect a candidate outside the
	// download tree while retaining a plausible Arr path.
	localPath := s.mapPath(file.Path)
	localRoot := s.mapPath(root)
	if !filepath.IsAbs(localPath) || !filepath.IsAbs(localRoot) || !pathutil.Within(pathutil.ResolveExisting(localRoot), pathutil.ResolveExisting(localPath)) {
		return checked, errors.New("manual import source resolves outside the download path")
	}
	localLibrary := s.mapPath(libraryPath)
	if libraryPath == "" || (filepath.IsAbs(localLibrary) && pathutil.Within(pathutil.ResolveExisting(localLibrary), pathutil.ResolveExisting(localPath))) {
		return checked, errors.New("manual import source resolves inside or has no known library root")
	}
	validation, localPath, err := s.validate(ctx, file)
	if err != nil {
		return checked, fmt.Errorf("manual import source validation: %w", err)
	}
	if !validation.Valid {
		return checked, fmt.Errorf("manual import source failed subtitle validation: %s", validation.Reason)
	}
	checked.request, checked.localPath, checked.validation = request, localPath, validation
	checked.expect = ImportExpectation{Source: candidate.Path, Size: candidate.Size, EpisodeIDs: request.EpisodeIDs, Year: file.Year}
	return checked, nil
}

func (s *Service) importExhaustedQueue(ctx context.Context, c *arr.Client, item arr.QueueRecord, plan queueRecoveryPlan, keys []string) error {
	if s.config.MaxAttempts < 1 || len(keys) == 0 {
		return errors.New("manual import requires a valid exhausted retry budget")
	}
	for _, key := range keys {
		if s.state.Attempts(key) < s.config.MaxAttempts {
			return errors.New("manual import requires exhausted attempts for every selected target")
		}
	}
	root, err := s.checkImportQueue(ctx, c, item, plan, true)
	if err != nil {
		return err
	}
	candidates, err := c.ImportCandidates(ctx, item.DownloadID)
	if err != nil {
		return fmt.Errorf("manual import discovery: %w", err)
	}
	var checked []checkedImport
	covered := map[int]bool{}
	paths := map[string]bool{}
	for _, candidate := range candidates {
		// A completed part of a pack may remain in the client directory. It is
		// never selected, and files spanning both present and missing episodes fail.
		if c.Kind() == "sonarr" && candidate.Series != nil && candidate.Series.ID == plan.subjectID && len(candidate.Episodes) > 0 {
			ids, err := importEpisodeIDs(candidate.Episodes, plan.subjectID)
			if err != nil {
				return err
			}
			existingOnly := true
			for _, id := range ids {
				if !slices.Contains(plan.episodeIDs, id) {
					return errors.New("manual import pack contains an unexpected episode")
				}
				if slices.Contains(plan.searchIDs, id) {
					existingOnly = false
				}
			}
			if existingOnly {
				continue
			}
		}
		file, err := s.checkImportCandidate(ctx, c, item, plan, root, candidate)
		if err != nil {
			return err
		}
		key := pathutil.Key(pathutil.ResolveExisting(file.localPath))
		if paths[key] {
			return errors.New("manual import candidates duplicate the same source")
		}
		paths[key] = true
		for _, id := range file.expect.EpisodeIDs {
			if covered[id] {
				return errors.New("manual import candidates overlap episode targets")
			}
			covered[id] = true
		}
		checked = append(checked, file)
	}
	if (c.Kind() == "radarr" && len(checked) != 1) || (c.Kind() == "sonarr" && (len(checked) == 0 || len(covered) != len(plan.searchIDs))) {
		return errors.New("manual import candidates do not uniquely cover every missing target")
	}
	// Recheck decisions, ownership and still-missing targets after slow probes.
	fresh, err := c.ImportCandidates(ctx, item.DownloadID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(candidates, fresh) {
		return deferProcessing(errors.New("manual import candidates changed during validation"))
	}
	freshPlan, err := s.planQueueRecovery(ctx, c, item, map[string]bool{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan, freshPlan) {
		return deferProcessing(errors.New("manual import targets changed during validation"))
	}
	freshRoot, err := s.checkImportQueue(ctx, c, item, plan, true)
	if err != nil {
		return err
	}
	if !sameArrPath(root, freshRoot) {
		return deferProcessing(errors.New("manual import download path changed"))
	}
	history, err := c.DownloadHistory(ctx, item.DownloadID)
	if err != nil {
		return err
	}
	pending := &PendingImport{OutputPath: root}
	for _, h := range history {
		if !strings.EqualFold(h.DownloadID, item.DownloadID) || (c.Kind() == "radarr" && h.MovieID != plan.subjectID) || (c.Kind() == "sonarr" && h.SeriesID != plan.subjectID) {
			return errors.New("manual import history changed subject or download")
		}
		pending.HistoryAfter = max(pending.HistoryAfter, h.ID)
	}
	var files []arr.ManualImportFile
	for _, file := range checked {
		if !pathutil.Within(pathutil.ResolveExisting(s.mapPath(root)), pathutil.ResolveExisting(file.localPath)) {
			return deferProcessing(errors.New("manual import source location changed after validation"))
		}
		info, err := os.Stat(file.localPath)
		if err != nil || !pathutil.SameFile(file.validation.FileInfo, info) {
			return deferProcessing(errors.New("manual import source changed after validation"))
		}
		if err := file.validation.CheckSubtitleSnapshot(file.localPath); err != nil {
			return err
		}
		files = append(files, file.request)
		pending.Files = append(pending.Files, file.expect)
	}
	s.log.Info("exhausted matched-by-ID import planned", "arr", c.Kind(), "queue_id", item.ID, "files", len(files), "dry_run", s.config.DryRun)
	if s.config.DryRun || c.IsReadOnly() || s.config.Mode == "unmatched" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := c.Kind() + ":queue:" + strings.ToLower(item.DownloadID)
	op := Operation{Kind: c.Kind(), SubjectID: plan.subjectID, DownloadID: item.DownloadID, EpisodeIDs: plan.searchIDs, Phase: "manual-import-requested", Import: pending}
	_, started, err := s.state.Begin(key, op, nil)
	if err != nil || !started {
		return err
	}
	command, err := c.ManualImport(ctx, files)
	if err != nil {
		return requireReconciliation(fmt.Errorf("manual import submission outcome is uncertain: %w", err))
	}
	if err := s.state.ImportSubmitted(key, command.ID); err != nil {
		return requireReconciliation(err)
	}
	s.log.Info("manual import submitted; awaiting verified library files", "arr", c.Kind(), "queue_id", item.ID, "command_id", command.ID)
	// Later queue scans/startup/serve ticks verify completion through reads if the
	// command is still running. Verified residual cleanup has its own journaled API request.
	return s.reconcileManualImport(ctx, c, key, s.state.Pending()[key])
}

func (s *Service) reconcileManualImport(ctx context.Context, c *arr.Client, key string, op Operation) error {
	if op.Phase == "manual-import-cleanup-requested" {
		return requireReconciliation(errors.New("manual import cleanup outcome is uncertain; automatic removal replay disabled"))
	}
	if op.Import == nil {
		return requireReconciliation(errors.New("manual import journal is missing"))
	}
	if op.Import.CommandID > 0 {
		command, err := c.GetImportCommand(ctx, op.Import.CommandID)
		var httpErr *arr.HTTPError
		if err != nil && (!errors.As(err, &httpErr) || httpErr.Status != 404) {
			return fmt.Errorf("read manual import command: %w", err)
		}
		if err == nil && (command.Status == "queued" || command.Status == "started") {
			return deferProcessing(errors.New("manual import command is still running"))
		}
	}
	// Arr can prune commands, or a timeout can lose the acknowledgement. New
	// import history plus verified current files can still establish completion;
	// no missing/failed command ever authorizes resubmission.
	history, err := c.SubjectHistory(ctx, op.SubjectID)
	if err != nil {
		return err
	}
	var validations []checkedImport
	for _, expected := range op.Import.Files {
		fileID, ids := 0, []int{}
		for _, h := range history {
			if h.ID <= op.Import.HistoryAfter || !strings.EqualFold(h.EventType, "downloadFolderImported") || !strings.EqualFold(h.DownloadID, op.DownloadID) || !sameArrPath(historyData(h, "droppedPath"), expected.Source) {
				continue
			}
			id, err := strconv.Atoi(historyData(h, "fileId"))
			if err != nil || id < 1 || (fileID != 0 && fileID != id) || (c.Kind() == "radarr" && h.MovieID != op.SubjectID) || (c.Kind() == "sonarr" && h.SeriesID != op.SubjectID) {
				return requireReconciliation(errors.New("manual import history has conflicting file identities"))
			}
			fileID = id
			if c.Kind() == "sonarr" {
				ids = append(ids, h.EpisodeID)
			}
		}
		if fileID == 0 || (c.Kind() == "sonarr" && !slices.Equal(arr.CanonicalIDs(ids), expected.EpisodeIDs)) {
			return deferProcessing(errors.New("manual import history is not yet verified"))
		}
		file, err := c.GetMediaFile(ctx, fileID)
		if err != nil {
			return err
		}
		if file.SubjectID(c.Kind()) != op.SubjectID || file.Size != expected.Size {
			return requireReconciliation(errors.New("manual import library file identity or size differs"))
		}
		if c.Kind() == "sonarr" {
			ids, err := c.EpisodeIDsForFile(ctx, op.SubjectID, fileID)
			if err != nil {
				return err
			}
			if !slices.Equal(arr.CanonicalIDs(ids), expected.EpisodeIDs) {
				return requireReconciliation(errors.New("manual import library episode mapping differs"))
			}
		}
		file.Year = expected.Year
		validation, localPath, err := s.validate(ctx, file)
		if err != nil {
			return err
		}
		if !validation.Valid {
			return requireReconciliation(errors.New("manual import library file failed subtitle validation"))
		}
		validations = append(validations, checkedImport{managed: file, expect: expected, validation: validation, localPath: localPath})
	}
	if len(validations) == 0 {
		return requireReconciliation(errors.New("manual import journal has no expected files"))
	}
	for _, v := range validations {
		fresh, err := c.GetMediaFile(ctx, v.managed.ID)
		if err != nil {
			return err
		}
		if !sameMedia(v.managed, fresh, c.Kind()) {
			return deferProcessing(errors.New("manual import library ownership changed during verification"))
		}
		if c.Kind() == "sonarr" {
			ids, err := c.EpisodeIDsForFile(ctx, op.SubjectID, v.managed.ID)
			if err != nil {
				return err
			}
			if !slices.Equal(arr.CanonicalIDs(ids), v.expect.EpisodeIDs) {
				return deferProcessing(errors.New("manual import episode mapping changed during verification"))
			}
		}
		info, err := os.Stat(v.localPath)
		if err != nil || !pathutil.SameFile(v.validation.FileInfo, info) {
			return deferProcessing(errors.New("manual import library file changed during verification"))
		}
		if err := v.validation.CheckSubtitleSnapshot(v.localPath); err != nil {
			return err
		}
	}
	if s.config.DryRun || c.IsReadOnly() || s.config.Mode == "unmatched" {
		return nil
	}
	if err := s.cleanupManualImport(ctx, c, key, op); err != nil {
		return err
	}
	// Completion is recorded only after actual library validation and any needed
	// residual queue cleanup. No blocklist, search or retry increment is performed.
	if err := s.state.Complete(key); err != nil {
		return err
	}
	s.log.Info("manual import verified", "arr", c.Kind(), "subject_id", op.SubjectID, "command_id", op.Import.CommandID, "files", len(validations))
	return nil
}

func (s *Service) cleanupManualImport(ctx context.Context, c *arr.Client, key string, op Operation) error {
	queue, err := c.Queue(ctx)
	if err != nil {
		return err
	}
	var blocked *arr.QueueRecord
	for _, q := range queue {
		if !strings.EqualFold(q.DownloadID, op.DownloadID) {
			continue
		}
		if !strings.EqualFold(q.Status, "completed") || (!q.NeedsImportRecovery() && !strings.EqualFold(q.TrackedDownloadState, "imported")) {
			return deferProcessing(errors.New("manual import download still has active or importable consumers"))
		}
		if q.NeedsImportRecovery() && blocked == nil {
			copy := q
			blocked = &copy
		}
	}
	// A normal completed import is cleaned up by Arr. Partial manual imports can
	// leave a rejected queue row because Arr counts only this command's files.
	if blocked == nil {
		return nil
	}
	if !s.config.RecoverBlockedQueue {
		return deferProcessing(errors.New("manual import residual cleanup is disabled by RECOVER_BLOCKED_QUEUE"))
	}
	plan, err := s.planQueueRecovery(ctx, c, *blocked, map[string]bool{})
	if err != nil {
		return err
	}
	if len(plan.rows) == 0 {
		return nil
	}
	if plan.subjectID != op.SubjectID {
		return requireReconciliation(errors.New("manual import cleanup subject changed"))
	}
	_, needed, err := s.remainingSearchTargets(ctx, c, plan.subjectID, plan.episodeIDs)
	if err != nil {
		return err
	}
	if needed {
		return deferProcessing(errors.New("manual import cleanup is waiting for all pack targets to have files"))
	}
	root, err := s.checkImportQueue(ctx, c, *blocked, plan, false)
	if err != nil {
		return err
	}
	if !sameArrPath(root, op.Import.OutputPath) {
		return requireReconciliation(errors.New("manual import cleanup output path changed"))
	}
	_, needed, err = s.remainingSearchTargets(ctx, c, plan.subjectID, plan.episodeIDs)
	if err != nil {
		return err
	}
	if needed {
		return deferProcessing(errors.New("manual import cleanup targets changed"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.state.Phase(key, "manual-import-cleanup-requested"); err != nil {
		return err
	}
	if err := c.RemoveImportedQueueItem(ctx, blocked.ID); err != nil {
		return requireReconciliation(fmt.Errorf("manual import cleanup outcome is uncertain: %w", err))
	}
	s.log.Info("verified manual import residual download removed", "arr", c.Kind(), "queue_id", blocked.ID, "remove_from_client", true, "blocklist", false)
	return nil
}

func (s *Service) reconcileManualImports(ctx context.Context, only *arr.Client) error {
	if s.state == nil {
		return nil
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	var firstErr error
	for key, op := range s.state.Pending() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if op.Import == nil || (only != nil && op.Kind != only.Kind()) {
			continue
		}
		client := only
		if client == nil {
			client = s.arr[op.Kind]
		}
		if client == nil {
			continue
		}
		if err := s.reconcileManualImport(ctx, client, key, op); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.log.Debug("manual import awaiting reconciliation", "arr", op.Kind, "command_id", op.Import.CommandID, "error", err)
		}
	}
	return firstErr
}

func validateImportJournal(key string, op Operation) error {
	if op.Import == nil {
		if strings.HasPrefix(op.Phase, "manual-import-") {
			return errors.New("manual import journal is missing its expected files")
		}
		return nil
	}
	if (op.Phase != "manual-import-requested" && op.Phase != "manual-import-submitted" && op.Phase != "manual-import-cleanup-requested") || op.DownloadID == "" || key != op.Kind+":queue:"+strings.ToLower(op.DownloadID) || op.Import.CommandID < 0 || (op.Phase == "manual-import-submitted" && op.Import.CommandID == 0) || op.Import.HistoryAfter < 1 || op.Import.OutputPath == "" || len(op.Import.Files) == 0 {
		return errors.New("invalid manual import journal")
	}
	var ids []int
	for _, file := range op.Import.Files {
		if file.Size < 1 || file.Source == "" || file.Source != strings.TrimSpace(file.Source) || !pathutil.IsMedia(file.Source) {
			return errors.New("invalid manual import expected file")
		}
		if op.Kind == "sonarr" && len(file.EpisodeIDs) == 0 {
			return errors.New("manual import journal has no episode mapping")
		}
		for _, id := range file.EpisodeIDs {
			if id < 1 {
				return errors.New("manual import journal has an invalid episode")
			}
			ids = append(ids, id)
		}
	}
	if op.Kind == "radarr" && (len(op.Import.Files) != 1 || len(ids) != 0) {
		return errors.New("manual import journal has ambiguous movie targets")
	}
	if len(ids) != len(arr.CanonicalIDs(ids)) || !slices.Equal(arr.CanonicalIDs(ids), op.EpisodeIDs) {
		return errors.New("manual import journal has conflicting episode targets")
	}
	return nil
}
