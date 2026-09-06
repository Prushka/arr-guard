package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type ArrClient struct {
	config   ArrConfig
	client   *http.Client
	log      *slog.Logger
	readOnly bool
}

func NewArrClient(config ArrConfig, log *slog.Logger) *ArrClient {
	return &ArrClient{
		config: config,
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		log:    log.With("arr", config.Name),
	}
}

func (c *ArrClient) Kind() string { return c.config.Kind }

func (c *ArrClient) apiPath(parts ...string) string {
	all := append([]string{"api", c.config.APIVersion}, parts...)
	return c.config.URL + "/" + path.Join(all...)
}

func (c *ArrClient) do(ctx context.Context, method, endpoint string, query url.Values, body any, out any) error {
	if c.readOnly && method != http.MethodGet {
		return errors.New("read-only Arr client refuses mutation")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	requestURL := endpoint
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.config.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, requestURL, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ArrHTTPError{Method: method, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return errors.New("arr returned no content for a resource read")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return fmt.Errorf("read Arr response: %w", err)
	}
	if len(data) > 16<<20 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("arr response is oversized or null")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", requestURL, err)
	}
	return nil
}

func (c *ArrClient) Test(ctx context.Context) error {
	var status struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, c.apiPath("system", "status"), nil, nil, &status); err != nil {
		return err
	}
	if !strings.EqualFold(status.AppName, c.Kind()) || status.Version == "" {
		return errors.New("unexpected application or missing version in Arr status")
	}
	c.log.Info("connected", "app", status.AppName, "version", status.Version, "api", c.config.APIVersion)
	return nil
}

func (c *ArrClient) ListLibraryFiles(ctx context.Context) ([]MediaFile, error) {
	if c.Kind() == "sonarr" {
		return c.listSonarrFiles(ctx, false)
	}
	return c.listRadarrFiles(ctx)
}

// ListSubtitleGuardFiles includes Sonarr episode release dates so the subtitle
// guard can exclude only episode files whose contained episodes are all old
// enough for the silent-media exclusion. Unmatched scans deliberately use
// ListLibraryFiles instead because they only need Arr media-file paths.
func (c *ArrClient) ListSubtitleGuardFiles(ctx context.Context) ([]MediaFile, error) {
	if c.Kind() == "sonarr" {
		return c.listSonarrFiles(ctx, true)
	}
	return c.listRadarrFiles(ctx)
}

func (c *ArrClient) listSonarrFiles(ctx context.Context, includeEpisodeReleaseYears bool) ([]MediaFile, error) {
	var series []Series
	if err := c.do(ctx, http.MethodGet, c.apiPath("series"), nil, nil, &series); err != nil {
		return nil, err
	}
	files := make([]MediaFile, 0)
	for _, item := range series {
		var group []MediaFile
		query := url.Values{"seriesId": {strconv.Itoa(item.ID)}}
		if err := c.do(ctx, http.MethodGet, c.apiPath("episodefile"), query, nil, &group); err != nil {
			return nil, fmt.Errorf("list files for series %d: %w", item.ID, err)
		}
		releaseYears := map[int]int(nil)
		if includeEpisodeReleaseYears {
			episodes, err := c.sonarrEpisodes(ctx, item.ID)
			if err != nil {
				return nil, fmt.Errorf("list episodes for series %d: %w", item.ID, err)
			}
			releaseYears = latestEpisodeReleaseYearByFile(episodes)
		}
		for i := range group {
			if group[i].ID < 1 || group[i].ParentID != item.ID {
				return nil, errors.New("sonarr file list has mismatched identity")
			}
			group[i].ParentID = item.ID
			if releaseYear := releaseYears[group[i].ID]; releaseYear > 0 {
				// A multi-episode file must remain guarded when it contains any
				// episode that is not old enough for the silent-media exclusion.
				group[i].Year = releaseYear
			} else if !includeEpisodeReleaseYears && group[i].Year == 0 {
				group[i].Year = item.Year
			} else if includeEpisodeReleaseYears {
				group[i].Year = 0
			}
			if group[i].Path == "" {
				if item.Path == "" || group[i].RelativePath == "" {
					return nil, errors.New("sonarr file has no complete path")
				}
				group[i].Path = joinArrPath(item.Path, group[i].RelativePath)
			}
		}
		files = append(files, group...)
	}
	return files, nil
}

func (c *ArrClient) sonarrEpisodes(ctx context.Context, seriesID int) ([]Episode, error) {
	if seriesID < 1 {
		return nil, errors.New("series ID must be positive")
	}
	query := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	var episodes []Episode
	if err := c.do(ctx, http.MethodGet, c.apiPath("episode"), query, nil, &episodes); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, episode := range episodes {
		if episode.ID < 1 || episode.SeriesID != seriesID || episode.EpisodeFileID < 0 || seen[episode.ID] {
			return nil, errors.New("episode list has invalid or mismatched identity")
		}
		seen[episode.ID] = true
	}
	return episodes, nil
}

func latestEpisodeReleaseYearByFile(episodes []Episode) map[int]int {
	years := make(map[int]int)
	unknown := make(map[int]bool)
	for _, episode := range episodes {
		if episode.EpisodeFileID < 1 {
			continue
		}
		if episode.ReleaseYear() == 0 {
			unknown[episode.EpisodeFileID] = true
		}
		if releaseYear := episode.ReleaseYear(); releaseYear > years[episode.EpisodeFileID] {
			years[episode.EpisodeFileID] = releaseYear
		}
	}
	for id := range unknown {
		years[id] = 0
	}
	return years
}

func (c *ArrClient) EpisodeReleaseYearForFile(ctx context.Context, seriesID, fileID int) (int, error) {
	if c.Kind() != "sonarr" {
		return 0, errors.New("episode release year is only available for Sonarr")
	}
	if seriesID < 1 || fileID < 1 {
		return 0, errors.New("series ID and episode file ID are required")
	}
	episodes, err := c.sonarrEpisodes(ctx, seriesID)
	if err != nil {
		return 0, err
	}
	return latestEpisodeReleaseYearByFile(episodes)[fileID], nil
}

func (c *ArrClient) listRadarrFiles(ctx context.Context) ([]MediaFile, error) {
	var movies []Movie
	if err := c.do(ctx, http.MethodGet, c.apiPath("movie"), nil, nil, &movies); err != nil {
		return nil, err
	}
	files := make([]MediaFile, 0)
	for _, item := range movies {
		var group []MediaFile
		query := url.Values{"movieId": {strconv.Itoa(item.ID)}}
		if err := c.do(ctx, http.MethodGet, c.apiPath("moviefile"), query, nil, &group); err != nil {
			return nil, fmt.Errorf("list files for movie %d: %w", item.ID, err)
		}
		for i := range group {
			if group[i].ID < 1 || group[i].MovieID != item.ID {
				return nil, errors.New("radarr file list has mismatched identity")
			}
			group[i].MovieID = item.ID
			group[i].Year = item.Year
			if group[i].Path == "" {
				if item.Path == "" || group[i].RelativePath == "" {
					return nil, errors.New("radarr file has no complete path")
				}
				group[i].Path = joinArrPath(item.Path, group[i].RelativePath)
			}
		}
		files = append(files, group...)
	}
	return files, nil
}

func (c *ArrClient) GetMediaFile(ctx context.Context, id int) (MediaFile, error) {
	if id < 1 {
		return MediaFile{}, errors.New("media file ID must be positive")
	}
	var result MediaFile
	resource := "episodefile"
	if c.Kind() == "radarr" {
		resource = "moviefile"
	}
	err := c.do(ctx, http.MethodGet, c.apiPath(resource, strconv.Itoa(id)), nil, nil, &result)
	if err == nil && (result.ID != id || result.SubjectID(c.Kind()) < 1) {
		err = errors.New("arr returned mismatched media identity")
	}
	return result, err
}

// EpisodeIDsForFile resolves the episodes that point at one Sonarr episode
// file. Sonarr's episode-file resource does not include those IDs, so the
// episode resource is the authoritative mapping for audit jobs and webhooks
// that do not carry an Episodes array.
func (c *ArrClient) EpisodeIDsForFile(ctx context.Context, seriesID, fileID int) ([]int, error) {
	if c.Kind() != "sonarr" {
		return nil, fmt.Errorf("episode IDs are only available for Sonarr")
	}
	if seriesID < 1 || fileID < 1 {
		return nil, fmt.Errorf("series ID and episode file ID are required")
	}
	episodes, err := c.sonarrEpisodes(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0)
	for _, episode := range episodes {
		if episode.ID > 0 && episode.EpisodeFileID == fileID {
			ids = append(ids, episode.ID)
		}
	}
	return canonicalIDs(ids), nil
}

func (c *ArrClient) DeleteMediaFile(ctx context.Context, id int) error {
	if id < 1 {
		return errors.New("media file ID must be positive")
	}
	resource := "episodefile"
	if c.Kind() == "radarr" {
		resource = "moviefile"
	}
	return c.do(ctx, http.MethodDelete, c.apiPath(resource, strconv.Itoa(id)), nil, nil, nil)
}

func (c *ArrClient) SubjectHistory(ctx context.Context, subjectID int) ([]HistoryRecord, error) {
	if subjectID < 1 {
		return nil, errors.New("history subject ID must be positive")
	}
	var records []HistoryRecord
	resource := "series"
	key := "seriesId"
	if c.Kind() == "radarr" {
		resource = "movie"
		key = "movieId"
	}
	query := url.Values{key: {strconv.Itoa(subjectID)}}
	if err := c.do(ctx, http.MethodGet, c.apiPath("history", resource), query, nil, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func readArrPages[T any](ctx context.Context, c *ArrClient, resource string, query url.Values, id func(T) int) ([]T, error) {
	all := []T{}
	seen := map[int]bool{}
	query.Set("pageSize", "1000")
	for number := 1; number <= 10000; number++ {
		query.Set("page", strconv.Itoa(number))
		var page struct {
			Records      []T `json:"records"`
			TotalRecords int `json:"totalRecords"`
		}
		if err := c.do(ctx, http.MethodGet, c.apiPath(resource), query, nil, &page); err != nil {
			return nil, err
		}
		if page.Records == nil || page.TotalRecords < 0 {
			return nil, errors.New("arr returned an invalid page")
		}
		for _, record := range page.Records {
			key := id(record)
			if key < 1 || seen[key] {
				return nil, errors.New("arr pagination has missing or repeated IDs; retry with a stable snapshot")
			}
			seen[key] = true
			all = append(all, record)
		}
		if page.TotalRecords == 0 || len(all) >= page.TotalRecords {
			return all, nil
		}
		if len(page.Records) == 0 {
			return nil, errors.New("arr pagination ended before totalRecords")
		}
	}
	return nil, errors.New("arr pagination exceeded safety limit")
}

func (c *ArrClient) DownloadHistory(ctx context.Context, downloadID string) ([]HistoryRecord, error) {
	if strings.TrimSpace(downloadID) == "" {
		return nil, errors.New("download ID is required")
	}
	return readArrPages(ctx, c, "history", url.Values{"downloadId": {downloadID}, "sortKey": {"date"}, "sortDirection": {"descending"}}, func(h HistoryRecord) int { return h.ID })
}

func (c *ArrClient) Queue(ctx context.Context) ([]QueueRecord, error) {
	return readArrPages(ctx, c, "queue", url.Values{"sortKey": {"timeleft"}, "sortDirection": {"ascending"}}, func(q QueueRecord) int { return q.ID })
}
func (c *ArrClient) FailQueueItem(ctx context.Context, id int, message string) error {
	if id < 1 {
		return errors.New("queue ID must be positive")
	}
	query := url.Values{
		"removeFromClient": {"true"},
		"blocklist":        {"true"},
		// The sidecar submits the replacement search explicitly after this
		// request, so avoid starting a second automatic search here.
		"skipRedownload": {"true"},
	}
	if c.Kind() == "sonarr" && message != "" {
		query.Set("message", message)
	}
	return c.do(ctx, http.MethodDelete, c.apiPath("queue", strconv.Itoa(id)), query, nil, nil)
}

func (c *ArrClient) MarkHistoryFailed(ctx context.Context, id int) error {
	if id < 1 {
		return errors.New("history ID must be positive")
	}
	// The v3 history endpoint does not expose skipRedownload; the explicit
	// search submitted by the service guarantees a replacement even when Arr's
	// automatic failed-download redownload setting is disabled.
	return c.do(ctx, http.MethodPost, c.apiPath("history", "failed", strconv.Itoa(id)), nil, nil, nil)
}

func (c *ArrClient) SearchEpisodes(ctx context.Context, subjectID int, episodeIDs []int) error {
	if subjectID < 1 {
		return errors.New("search subject ID must be positive")
	}
	for _, id := range episodeIDs {
		if id < 1 {
			return errors.New("episode ID must be positive")
		}
	}
	command := CommandRequest{}
	if c.Kind() == "sonarr" {
		if len(episodeIDs) == 0 {
			return errors.New("sonarr episode search requires at least one episode ID")
		}
		command.Name = "EpisodeSearch"
		command.EpisodeIDs = episodeIDs
	} else {
		command.Name = "MoviesSearch"
		command.MovieIDs = []int{subjectID}
	}
	return c.do(ctx, http.MethodPost, c.apiPath("command"), nil, command, nil)
}

func joinArrPath(parent, relative string) string {
	parent = strings.TrimSpace(parent)
	relative = strings.TrimSpace(relative)
	if parent == "" {
		return relative
	}
	if relative == "" {
		return parent
	}
	if strings.Contains(parent, "\\") {
		return strings.TrimRight(parent, "\\/") + "\\" + strings.TrimLeft(relative, "\\/")
	}
	return strings.TrimRight(parent, "/") + "/" + strings.TrimLeft(relative, "/")
}
