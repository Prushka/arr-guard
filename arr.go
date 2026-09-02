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
	config ArrConfig
	client *http.Client
	log    *slog.Logger
}

func NewArrClient(config ArrConfig, log *slog.Logger) *ArrClient {
	return &ArrClient{
		config: config,
		client: &http.Client{Timeout: 30 * time.Second},
		log:    log.With("arr", config.Name),
	}
}

func (c *ArrClient) Kind() string { return c.config.Kind }

func (c *ArrClient) apiPath(parts ...string) string {
	all := append([]string{"api", c.config.APIVersion}, parts...)
	return c.config.URL + "/" + path.Join(all...)
}

func (c *ArrClient) do(ctx context.Context, method, endpoint string, query url.Values, body any, out any) error {
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, requestURL, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
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
	c.log.Info("connected", "app", status.AppName, "version", status.Version, "api", c.config.APIVersion)
	return nil
}

func (c *ArrClient) ListLibraryFiles(ctx context.Context) ([]MediaFile, error) {
	if c.Kind() == "sonarr" {
		return c.listSonarrFiles(ctx)
	}
	return c.listRadarrFiles(ctx)
}

func (c *ArrClient) listSonarrFiles(ctx context.Context) ([]MediaFile, error) {
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
		for i := range group {
			group[i].ParentID = item.ID
			if group[i].Year == 0 {
				group[i].Year = item.Year
			}
			if group[i].Path == "" {
				group[i].Path = joinArrPath(item.Path, group[i].RelativePath)
			}
		}
		files = append(files, group...)
	}
	return files, nil
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
			group[i].MovieID = item.ID
			if group[i].Year == 0 {
				group[i].Year = item.Year
			}
			if group[i].Path == "" {
				group[i].Path = joinArrPath(item.Path, group[i].RelativePath)
			}
		}
		files = append(files, group...)
	}
	return files, nil
}

func (c *ArrClient) GetMediaFile(ctx context.Context, id int) (MediaFile, error) {
	var result MediaFile
	resource := "episodefile"
	if c.Kind() == "radarr" {
		resource = "moviefile"
	}
	err := c.do(ctx, http.MethodGet, c.apiPath(resource, strconv.Itoa(id)), nil, nil, &result)
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
	query := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	var episodes []Episode
	if err := c.do(ctx, http.MethodGet, c.apiPath("episode"), query, nil, &episodes); err != nil {
		return nil, err
	}
	ids := make([]int, 0)
	for _, episode := range episodes {
		if episode.ID > 0 && episode.EpisodeFileID == fileID {
			ids = append(ids, episode.ID)
		}
	}
	return ids, nil
}

func (c *ArrClient) DeleteMediaFile(ctx context.Context, id int) error {
	resource := "episodefile"
	if c.Kind() == "radarr" {
		resource = "moviefile"
	}
	return c.do(ctx, http.MethodDelete, c.apiPath(resource, strconv.Itoa(id)), nil, nil, nil)
}

func (c *ArrClient) SubjectHistory(ctx context.Context, subjectID int) ([]HistoryRecord, error) {
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

func (c *ArrClient) DownloadHistory(ctx context.Context, downloadID string) ([]HistoryRecord, error) {
	query := url.Values{
		"downloadId":    {downloadID},
		"page":          {"1"},
		"pageSize":      {"1000"},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
	}
	var page HistoryPage
	if err := c.do(ctx, http.MethodGet, c.apiPath("history"), query, nil, &page); err != nil {
		return nil, err
	}
	return page.Records, nil
}

func (c *ArrClient) Queue(ctx context.Context) ([]QueueRecord, error) {
	const pageSize = 1000
	all := make([]QueueRecord, 0)
	for pageNumber := 1; ; pageNumber++ {
		query := url.Values{"page": {strconv.Itoa(pageNumber)}, "pageSize": {strconv.Itoa(pageSize)}, "sortKey": {"timeleft"}, "sortDirection": {"ascending"}}
		var page QueuePage
		if err := c.do(ctx, http.MethodGet, c.apiPath("queue"), query, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Records...)
		if len(page.Records) == 0 || page.TotalRecords == 0 || len(all) >= page.TotalRecords {
			break
		}
	}
	return all, nil
}

func (c *ArrClient) FailQueueItem(ctx context.Context, id int, message string) error {
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
	// The v3 history endpoint does not expose skipRedownload; the explicit
	// search submitted by the service guarantees a replacement even when Arr's
	// automatic failed-download redownload setting is disabled.
	return c.do(ctx, http.MethodPost, c.apiPath("history", "failed", strconv.Itoa(id)), nil, nil, nil)
}

func (c *ArrClient) Search(ctx context.Context, subjectID int) error {
	if c.Kind() == "sonarr" {
		return c.SearchSeries(ctx, subjectID)
	}
	return c.SearchEpisodes(ctx, subjectID, nil)
}

func (c *ArrClient) SearchSeries(ctx context.Context, seriesID int) error {
	if c.Kind() != "sonarr" {
		return errors.New("series search is only available for Sonarr")
	}
	return c.do(ctx, http.MethodPost, c.apiPath("command"), nil, CommandRequest{Name: "SeriesSearch", SeriesID: seriesID}, nil)
}

func (c *ArrClient) SearchEpisodes(ctx context.Context, subjectID int, episodeIDs []int) error {
	command := CommandRequest{}
	if c.Kind() == "sonarr" {
		if len(episodeIDs) == 0 {
			return errors.New("Sonarr episode search requires at least one episode ID")
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
