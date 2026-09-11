package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

type queueRecoveryRow struct {
	id, episodeID int
	imported      bool
}

type queueRecoveryPlan struct {
	subjectID             int
	episodeIDs, searchIDs []int
	search                bool
	rows                  []queueRecoveryRow
}

// Every row sharing the download is relevant, including already imported rows.
// Pending rows without a selected rejection may still be importable.
func queueRecoveryGroup(kind string, item QueueRecord, queue []QueueRecord, historySubject int) (int, []queueRecoveryRow, error) {
	subject, found, rejected := 0, false, false
	var rows []queueRecoveryRow
	for _, q := range queue {
		if q.ID == item.ID && !strings.EqualFold(q.DownloadID, item.DownloadID) {
			return 0, nil, errors.New("queue ID was reassigned")
		}
		if !strings.EqualFold(q.DownloadID, item.DownloadID) {
			continue
		}
		found = found || q.ID == item.ID
		imported := strings.EqualFold(strings.TrimSpace(q.TrackedDownloadState), "imported")
		if !strings.EqualFold(strings.TrimSpace(q.Status), "completed") || (!q.needsImportRecovery() && !imported) {
			return 0, nil, errors.New("shared download still has active or importable items")
		}
		rejected = rejected || q.needsImportRecovery()
		current := q.MovieID
		if kind == "sonarr" {
			current = q.SeriesID
		}
		if current == 0 {
			current = historySubject
		}
		if q.ID < 1 || current < 1 || (subject != 0 && subject != current) || q.EpisodeID < 0 {
			return 0, nil, errors.New("shared download has ambiguous subject mapping")
		}
		subject = current
		rows = append(rows, queueRecoveryRow{q.ID, q.EpisodeID, imported})
	}
	if !found {
		return 0, nil, nil
	}
	if !rejected {
		return 0, nil, nil
	}
	expected := item.MovieID
	if kind == "sonarr" {
		expected = item.SeriesID
	}
	if subject != historySubject || (expected != 0 && subject != expected) {
		return 0, nil, errors.New("queue subject changed since scan")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return subject, rows, nil
}

func (s *Service) planQueueRecovery(ctx context.Context, c *ArrClient, item QueueRecord, searched map[string]bool) (queueRecoveryPlan, error) {
	queue, err := c.Queue(ctx)
	if err != nil {
		return queueRecoveryPlan{}, err
	}
	found := false
	for _, q := range queue {
		if q.ID == item.ID {
			if !strings.EqualFold(q.DownloadID, item.DownloadID) {
				return queueRecoveryPlan{}, errors.New("queue ID was reassigned")
			}
			found = true
		}
	}
	if !found {
		return queueRecoveryPlan{}, nil
	}
	plan := queueRecoveryPlan{}
	subject := 0
	history, err := c.DownloadHistory(ctx, item.DownloadID)
	if err != nil {
		return queueRecoveryPlan{}, err
	}
	grabbed := map[int]bool{}
	hasGrabbed := false
	var ids []int
	for _, h := range history {
		historySubject := h.MovieID
		if c.Kind() == "sonarr" {
			historySubject = h.SeriesID
		}
		if !strings.EqualFold(h.DownloadID, item.DownloadID) || historySubject < 1 || (subject != 0 && historySubject != subject) {
			return queueRecoveryPlan{}, errors.New("queue history crosses download or subject boundaries")
		}
		subject = historySubject
		if strings.EqualFold(h.EventType, "grabbed") || strings.EqualFold(h.EventType, "downloadFolderImported") {
			hasGrabbed = hasGrabbed || strings.EqualFold(h.EventType, "grabbed")
			if c.Kind() == "sonarr" {
				if h.EpisodeID < 1 {
					return queueRecoveryPlan{}, errors.New("queue history has incomplete episode mapping")
				}
				ids = append(ids, h.EpisodeID)
				if strings.EqualFold(h.EventType, "grabbed") {
					grabbed[h.EpisodeID] = true
				}
			}
		}
	}
	// Arr's queue blocklist action silently does nothing without grabbed history.
	if !hasGrabbed {
		return queueRecoveryPlan{}, deferProcessing(errors.New("queue recovery needs grabbed history to blocklist the release"))
	}
	_, rows, err := queueRecoveryGroup(c.Kind(), item, queue, subject)
	if err != nil || len(rows) == 0 {
		return queueRecoveryPlan{}, err
	}
	plan.subjectID, plan.rows = subject, rows
	if c.Kind() == "sonarr" {
		for _, row := range rows {
			if row.episodeID == 0 && len(grabbed) == 0 {
				return queueRecoveryPlan{}, errors.New("shared queue item has no episode mapping")
			}
			if row.episodeID > 0 {
				ids = append(ids, row.episodeID)
			}
		}
	}
	plan.episodeIDs = canonicalIDs(ids)
	remaining, needed, err := s.remainingSearchTargets(ctx, c, subject, plan.episodeIDs)
	if err != nil {
		return queueRecoveryPlan{}, err
	}
	for _, row := range rows {
		if row.imported && ((c.Kind() == "radarr" && needed) || (c.Kind() == "sonarr" && (row.episodeID == 0 || slices.Contains(remaining, row.episodeID)))) {
			return queueRecoveryPlan{}, errors.New("imported queue row has no managed file")
		}
	}
	plan.searchIDs, plan.search = queueSearchTargets(c.Kind(), subject, item.DownloadID, remaining, needed, queue, searched)
	return plan, nil
}

// Suppress searches already issued in this scan, or covered by another live
// download. Rejected downloads are not replacements for one another.
func queueSearchTargets(kind string, subject int, origin string, ids []int, needed bool, queue []QueueRecord, searched map[string]bool) ([]int, bool) {
	if !needed {
		return nil, false
	}
	covered := map[int]bool{}
	movieCovered := false
	if kind == "radarr" {
		movieCovered = searched[retryKeys(kind, MediaFile{MovieID: subject}, nil)[0]]
	}
	for _, q := range queue {
		if strings.EqualFold(q.DownloadID, origin) || q.needsImportRecovery() {
			continue
		}
		active := false
		switch strings.ToLower(strings.TrimSpace(q.Status)) {
		case "downloading", "queued", "paused", "delay":
			active = true
		case "completed":
			active = strings.EqualFold(q.TrackedDownloadState, "importPending") || strings.EqualFold(q.TrackedDownloadState, "importing")
		}
		if !active {
			continue
		}
		if kind == "sonarr" && q.SeriesID == subject {
			covered[q.EpisodeID] = true
			if q.EpisodeID == 0 {
				for _, id := range ids {
					covered[id] = true
				}
			}
		}
		if kind == "radarr" && q.MovieID == subject {
			movieCovered = true
		}
	}
	if kind == "radarr" {
		return nil, needed && !movieCovered
	}
	var remaining []int
	for _, id := range ids {
		keys := retryKeys(kind, MediaFile{ParentID: subject}, []int{id})
		if !covered[id] && !searched[keys[0]] {
			remaining = append(remaining, id)
		}
	}
	return remaining, len(remaining) > 0
}

func (s *Service) recoverBlockedQueueItem(ctx context.Context, c *ArrClient, item QueueRecord) error {
	return s.recoverQueueItem(ctx, c, item, map[string]bool{})
}

func (s *Service) recoverQueueItem(ctx context.Context, c *ArrClient, item QueueRecord, searched map[string]bool) (resultErr error) {
	if !item.needsImportRecovery() {
		return nil
	}
	if item.ID < 1 || strings.TrimSpace(item.DownloadID) == "" {
		return errors.New("queue/download identity is missing")
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	plan, err := s.planQueueRecovery(ctx, c, item, searched)
	if err != nil || len(plan.rows) == 0 {
		return err
	}
	// Recheck group membership/state after history and media reads. A new active
	// sibling, reassigned ID, or import in progress must stop before any write.
	queue, err := c.Queue(ctx)
	if err != nil {
		return err
	}
	subject, rows, err := queueRecoveryGroup(c.Kind(), item, queue, plan.subjectID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if subject != plan.subjectID || !slices.Equal(rows, plan.rows) {
		return deferProcessing(errors.New("queue group changed during recovery preflight"))
	}
	// Only shrink the initial search scope; a vanished existing file is separate
	// work and cannot consume an unreserved retry or enlarge this operation.
	remaining, needed, err := s.remainingSearchTargets(ctx, c, subject, plan.episodeIDs)
	if err != nil {
		return err
	}
	if c.Kind() == "sonarr" {
		plan.searchIDs = slices.DeleteFunc(plan.searchIDs, func(id int) bool { return !slices.Contains(remaining, id) })
	}
	plan.searchIDs, plan.search = queueSearchTargets(c.Kind(), subject, item.DownloadID, plan.searchIDs, plan.search && needed, queue, searched)
	keys := []string{}
	if plan.search {
		keys = retryKeys(c.Kind(), MediaFile{ParentID: subject, MovieID: subject}, plan.searchIDs)
	}
	s.log.Warn("blocked download recovery planned", "arr", c.Kind(), "queue_id", item.ID, "episodes", len(plan.episodeIDs), "search_episode_ids", plan.searchIDs, "search", plan.search, "remove_from_client", true, "dry_run", s.config.DryRun)
	if s.config.DryRun {
		for _, key := range keys {
			searched[key] = true
		}
		return nil
	}
	if s.config.MaxAttempts < 1 {
		return errors.New("invalid maximum attempts")
	}
	for _, key := range keys {
		if s.state.Attempts(key) >= s.config.MaxAttempts {
			return errors.New("blocked download retry limit reached; left untouched")
		}
	}
	key := c.Kind() + ":queue:" + strings.ToLower(item.DownloadID)
	op := Operation{Kind: c.Kind(), SubjectID: subject, DownloadID: item.DownloadID, EpisodeIDs: plan.episodeIDs, Phase: "queue-removal-requested"}
	_, started, err := s.state.Begin(key, op, keys)
	if err != nil || !started {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = requireReconciliation(resultErr)
		}
	}()
	if err := c.FailQueueItem(ctx, item.ID, "Subtitle Guard: rejected automatic import"); err != nil {
		return fmt.Errorf("queue removal outcome requires reconciliation: %w", err)
	}
	if err := s.state.Phase(key, "queue-removed"); err != nil {
		return err
	}
	if plan.search {
		queue, err := c.Queue(ctx)
		if err != nil {
			return err
		}
		plan.searchIDs, plan.search = queueSearchTargets(c.Kind(), subject, item.DownloadID, plan.searchIDs, plan.search, queue, searched)
	}
	if plan.search {
		for _, key := range keys {
			searched[key] = true
		}
		if err := s.searchRemaining(ctx, c, key, subject, plan.searchIDs); err != nil {
			return err
		}
	}
	return s.state.Complete(key)
}
