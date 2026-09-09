package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type ArrHTTPError struct {
	Method string
	Status int
}

func (e *ArrHTTPError) Error() string {
	return fmt.Sprintf("Arr %s returned HTTP %d", e.Method, e.Status)
}

func pathWithin(root, target string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// Resolve the existing ancestor too, so a not-yet-created JSON file cannot hide
// beneath a media mount reached through a symlink or Windows junction.
func resolveExistingPath(value string) string {
	value, err := filepath.Abs(value)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return resolved
	}
	parent := filepath.Dir(value)
	if parent == value {
		return value
	}
	return filepath.Join(resolveExistingPath(parent), filepath.Base(value))
}

func isSubtitlePath(path string) bool {
	_, ok := subtitleExtensions[strings.ToLower(filepath.Ext(path))]
	return ok
}

func comparableArrPath(value, from string) (string, string) {
	value, from = normalizePath(value), normalizePath(from)
	if strings.HasPrefix(from, "//") || (len(from) > 1 && from[1] == ':') {
		value, from = strings.ToLower(value), strings.ToLower(from)
	}
	return value, from
}

func mappedSuffix(value, from string) string {
	comparableValue, comparableFrom := comparableArrPath(value, from)
	if comparableValue == comparableFrom {
		return ""
	}
	// The number of path separators stays stable even when Unicode case folding
	// changes the byte length of a Windows source prefix.
	separators := strings.Count(strings.TrimRight(from, "/"), "/") + 1
	for i, c := range value {
		if c == '/' {
			separators--
			if separators == 0 {
				return value[i:]
			}
		}
	}
	return ""
}

func canonicalIDs(ids []int) []int {
	ids = append([]int(nil), ids...)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func retryKeys(kind string, file MediaFile, ids []int) []string {
	if kind == "radarr" {
		return []string{retryKey(kind, file, nil, file.Path)}
	}
	keys := []string{}
	for _, id := range canonicalIDs(ids) {
		keys = append(keys, retryKey(kind, file, []int{id}, file.Path))
	}
	return keys
}

func (s *StateStore) Bind(clients map[string]*ArrClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for kind, c := range clients {
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(c.config.URL)))
		if old := s.state.Instances[kind]; old != "" && old != identity {
			return errors.New("STATE_PATH belongs to a different Arr server; use a separate state file")
		}
	}
	return s.updateLocked(func(next *State) {
		for kind, c := range clients {
			next.Instances[kind] = fmt.Sprintf("%x", sha256.Sum256([]byte(c.config.URL)))
		}
	})
}

func sameMedia(a, b MediaFile, kind string) bool {
	return a.ID == b.ID && a.SubjectID(kind) == b.SubjectID(kind) && normalizePath(a.Path) == normalizePath(b.Path) && a.Size == b.Size && a.RelativePath == b.RelativePath && ((a.DateAdded == nil && b.DateAdded == nil) || (a.DateAdded != nil && b.DateAdded != nil && a.DateAdded.Equal(*b.DateAdded)))
}

func sameDiskFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// A prepared origin is read and validated before deleting a managed file.
type originAction struct {
	queueID, historyID, subjectID int
	downloadID                    string
	releaseSource                 string
	automaticSearch               bool
}

func (s *Service) prepareOrigin(ctx context.Context, c *ArrClient, file MediaFile, downloadID string) (originAction, error) {
	if downloadID == "" {
		return originAction{}, nil
	}
	queue, err := c.Queue(ctx)
	if err != nil {
		return originAction{}, err
	}
	queueID := 0
	var queuedEpisodes []int
	for _, q := range queue {
		if !strings.EqualFold(q.DownloadID, downloadID) {
			continue
		}
		if (c.Kind() == "radarr" && q.MovieID != file.MovieID) || (c.Kind() == "sonarr" && q.SeriesID != file.ParentID) {
			return originAction{}, errors.New("download is shared with another subject; refusing queue removal")
		}
		if !strings.EqualFold(q.Status, "completed") || !strings.EqualFold(q.TrackedDownloadState, "imported") || q.ID < 1 {
			return originAction{}, deferProcessing(errors.New("origin download is still importing or has no queue identity"))
		}
		if c.Kind() == "sonarr" {
			queuedEpisodes = append(queuedEpisodes, q.EpisodeID)
		}
		queueID = q.ID
	}
	if queueID > 0 {
		if c.Kind() == "sonarr" {
			episodes, err := c.sonarrEpisodes(ctx, file.ParentID)
			if err != nil {
				return originAction{}, err
			}
			imported := map[int]bool{}
			for _, e := range episodes {
				imported[e.ID] = e.EpisodeFileID > 0
			}
			for _, id := range queuedEpisodes {
				if !imported[id] {
					return originAction{}, deferProcessing(errors.New("shared download contains episodes without imported files"))
				}
			}
		}
		return originAction{queueID: queueID, subjectID: file.SubjectID(c.Kind()), downloadID: downloadID}, nil
	}
	records, err := c.DownloadHistory(ctx, downloadID)
	if err != nil {
		return originAction{}, err
	}
	var grabbed HistoryRecord
	alreadyFailed := false
	for _, h := range records {
		if !strings.EqualFold(h.DownloadID, downloadID) {
			return originAction{}, errors.New("download history filter mismatch")
		}
		if (c.Kind() == "radarr" && h.MovieID != file.MovieID) || (c.Kind() == "sonarr" && h.SeriesID != file.ParentID) {
			return originAction{}, errors.New("download history crosses subject boundaries")
		}
		if strings.EqualFold(h.EventType, "downloadFailed") {
			alreadyFailed = true
		}
		if strings.EqualFold(h.EventType, "grabbed") && h.ID > 0 {
			grabbed = h
		}
	}
	if alreadyFailed {
		return originAction{}, nil
	}
	if grabbed.ID == 0 {
		return originAction{}, deferProcessing(errors.New("no grabbed history for known originating download"))
	}
	automatic, err := c.AutomaticHistorySearch(ctx, grabbed.Data["releaseSource"])
	if err != nil {
		return originAction{}, err
	}
	return originAction{historyID: grabbed.ID, releaseSource: grabbed.Data["releaseSource"], automaticSearch: automatic}, nil
}

// Match Arr's RedownloadFailedDownloadService: the main setting gates every
// release, with an additional opt-out for releases grabbed by interactive search.
// Missing/invalid releaseSource is Unknown in Arr, not InteractiveSearch.
func (c *ArrClient) AutomaticHistorySearch(ctx context.Context, releaseSource string) (bool, error) {
	var cfg struct {
		Auto        *bool `json:"autoRedownloadFailed"`
		Interactive *bool `json:"autoRedownloadFailedFromInteractiveSearch"`
	}
	if err := c.do(ctx, http.MethodGet, c.apiPath("config", "downloadclient"), nil, nil, &cfg); err != nil {
		return false, err
	}
	if cfg.Auto == nil {
		return false, errors.New("arr automatic failed-redownload setting is missing")
	}
	if !*cfg.Auto {
		return false, nil
	}
	// Enum.TryParse in Arr accepts both the case-sensitive enum name and its
	// numeric value (InteractiveSearch = 4 in Sonarr and Radarr).
	releaseSource = strings.TrimSpace(releaseSource)
	numeric, _ := strconv.Atoi(releaseSource)
	if releaseSource == "InteractiveSearch" || numeric == 4 {
		if cfg.Interactive == nil {
			return false, errors.New("arr interactive failed-redownload setting is missing")
		}
		return *cfg.Interactive, nil
	}
	return true, nil
}

func (s *Service) applyOrigin(ctx context.Context, c *ArrClient, action originAction, reason string) error {
	if action.queueID > 0 {
		queue, err := c.Queue(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, q := range queue {
			if q.ID == action.queueID && !strings.EqualFold(q.DownloadID, action.downloadID) {
				return errors.New("origin queue ID was reassigned")
			}
			if !strings.EqualFold(q.DownloadID, action.downloadID) {
				continue
			}
			subjectID := q.MovieID
			if c.Kind() == "sonarr" {
				subjectID = q.SeriesID
			}
			if subjectID != action.subjectID || !strings.EqualFold(q.Status, "completed") || !strings.EqualFold(q.TrackedDownloadState, "imported") {
				return errors.New("origin queue changed after deletion; reconciliation required")
			}
			found = found || q.ID == action.queueID
		}
		if !found {
			return errors.New("origin queue disappeared after deletion; reconciliation required")
		}
		return c.FailQueueItem(ctx, action.queueID, "Subtitle Guard: "+reason)
	}
	if action.historyID > 0 {
		return c.MarkHistoryFailed(ctx, action.historyID)
	}
	return nil
}

func operationKey(kind string, fileID int) string { return kind + ":file:" + strconv.Itoa(fileID) }

func openServiceState(cfg Config) (*StateStore, error) {
	if cfg.Mode == "unmatched" {
		return &StateStore{state: State{Attempts: map[string]int{}}}, nil
	}
	cfg.StatePath = resolveExistingPath(cfg.StatePath)
	if cfg.DryRun {
		return LoadStateStore(cfg.StatePath)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o700); err != nil {
		return nil, err
	}
	f, err := lockStateFile(cfg.StatePath + ".lock")
	if err != nil {
		return nil, fmt.Errorf("STATE_PATH is locked or inaccessible: %w", err)
	}
	s, err := LoadStateStore(cfg.StatePath)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.lockFile = f
	return s, nil
}

func (s *Service) searchStillMissing(ctx context.Context, c *ArrClient, subjectID int, ids []int) error {
	remaining, needed, err := s.remainingSearchTargets(ctx, c, subjectID, ids)
	if err != nil {
		return err
	}
	if !needed || (c.Kind() == "sonarr" && len(remaining) != len(canonicalIDs(ids))) {
		return errors.New("queue recovery target has existing media; leaving shared download untouched")
	}
	return nil
}

func (s *Service) remainingSearchTargets(ctx context.Context, c *ArrClient, subjectID int, ids []int) ([]int, bool, error) {
	if c.Kind() == "sonarr" {
		episodes, err := c.sonarrEpisodes(ctx, subjectID)
		if err != nil {
			return nil, false, err
		}
		missing := map[int]bool{}
		for _, episode := range episodes {
			missing[episode.ID] = episode.EpisodeFileID == 0
		}
		var remaining []int
		for _, id := range canonicalIDs(ids) {
			isMissing, exists := missing[id]
			if !exists {
				return nil, false, errors.New("search target no longer exists; reconciliation required")
			}
			if isMissing {
				remaining = append(remaining, id)
			}
		}
		return remaining, len(remaining) > 0, nil
	} else {
		var movie Movie
		if err := c.do(ctx, http.MethodGet, c.apiPath("movie", strconv.Itoa(subjectID)), nil, nil, &movie); err != nil {
			return nil, false, err
		}
		if movie.ID != subjectID {
			return nil, false, errors.New("movie identity changed before search")
		}
		var files []MediaFile
		if err := c.do(ctx, http.MethodGet, c.apiPath("moviefile"), url.Values{"movieId": {strconv.Itoa(subjectID)}}, nil, &files); err != nil {
			return nil, false, err
		}
		for _, file := range files {
			if file.ID < 1 || file.MovieID != subjectID {
				return nil, false, errors.New("replacement movie file has mismatched identity")
			}
		}
		return nil, len(files) == 0, nil
	}
}

func (s *Service) searchRemaining(ctx context.Context, c *ArrClient, key string, subjectID int, ids []int) error {
	remaining, needed, err := s.remainingSearchTargets(ctx, c, subjectID, ids)
	if err != nil {
		return err
	}
	if !needed {
		s.log.Info("search skipped; targets already have replacement files", "arr", c.Kind(), "subject_id", subjectID)
		return nil
	}
	if err := s.state.SearchRequested(key, remaining); err != nil {
		return err
	}
	if err := c.SearchEpisodes(ctx, subjectID, remaining); err != nil {
		return fmt.Errorf("search outcome requires reconciliation: %w", err)
	}
	return nil
}
