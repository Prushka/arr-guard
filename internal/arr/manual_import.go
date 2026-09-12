package arr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Import metadata is kept as returned by Arr; Guard never invents quality,
// revision, language, or release attributes to bypass import decisions.
type ImportCandidate struct {
	ID            int               `json:"id"`
	Path          string            `json:"path"`
	FolderName    string            `json:"folderName"`
	Size          int64             `json:"size"`
	Movie         *Movie            `json:"movie"`
	Series        *Series           `json:"series"`
	Episodes      []Episode         `json:"episodes"`
	MovieFileID   int               `json:"movieFileId"`
	EpisodeFileID int               `json:"episodeFileId"`
	DownloadID    string            `json:"downloadId"`
	Quality       json.RawMessage   `json:"quality"`
	Languages     json.RawMessage   `json:"languages"`
	ReleaseGroup  string            `json:"releaseGroup"`
	IndexerFlags  int               `json:"indexerFlags"`
	ReleaseType   json.RawMessage   `json:"releaseType"`
	Rejections    []ImportRejection `json:"rejections"`
}

type ImportRejection struct {
	Reason string `json:"reason"`
}

type ParsedImport struct {
	Movie       *Movie          `json:"movie"`
	Series      *Series         `json:"series"`
	Episodes    []Episode       `json:"episodes"`
	MovieInfo   json.RawMessage `json:"parsedMovieInfo"`
	EpisodeInfo json.RawMessage `json:"parsedEpisodeInfo"`
}

type ManualImportFile struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName"`
	MovieID      int             `json:"movieId,omitempty"`
	SeriesID     int             `json:"seriesId,omitempty"`
	EpisodeIDs   []int           `json:"episodeIds,omitempty"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  json.RawMessage `json:"releaseType,omitempty"`
	DownloadID   string          `json:"downloadId"`
}

type ImportCommand struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (c *Client) ImportCandidates(ctx context.Context, downloadID string) ([]ImportCandidate, error) {
	if strings.TrimSpace(downloadID) == "" {
		return nil, errors.New("manual import requires a download ID")
	}
	var items []ImportCandidate
	// Do not pass movieId/seriesId: those can select library files instead of
	// downloads. GET only discovers candidates; POST /manualimport reprocesses
	// metadata and is not the command that actually imports files.
	err := c.do(ctx, http.MethodGet, c.apiPath("manualimport"), url.Values{"downloadId": {downloadID}, "filterExistingFiles": {"true"}}, nil, &items)
	if err == nil && items == nil {
		err = errors.New("manual import returned no candidate inventory")
	}
	return items, err
}

func (c *Client) ParseImportName(ctx context.Context, title string) (ParsedImport, error) {
	var parsed ParsedImport
	err := c.do(ctx, http.MethodGet, c.apiPath("parse"), url.Values{"title": {title}}, nil, &parsed)
	return parsed, err
}

func (c *Client) ManualImport(ctx context.Context, files []ManualImportFile) (ImportCommand, error) {
	var command ImportCommand
	if len(files) == 0 {
		return command, errors.New("manual import requires selected files")
	}
	request := struct {
		Name       string             `json:"name"`
		ImportMode string             `json:"importMode"`
		Files      []ManualImportFile `json:"files"`
	}{"ManualImport", "auto", files}
	err := c.do(ctx, http.MethodPost, c.apiPath("command"), nil, request, &command)
	if err == nil && (command.ID < 1 || command.Name != "ManualImport") {
		err = errors.New("manual import returned an invalid command acknowledgement")
	}
	return command, err
}

func (c *Client) GetImportCommand(ctx context.Context, id int) (ImportCommand, error) {
	var command ImportCommand
	if id < 1 {
		return command, errors.New("invalid import command ID")
	}
	err := c.do(ctx, http.MethodGet, c.apiPath("command", strconv.Itoa(id)), nil, nil, &command)
	if err == nil && (command.ID != id || command.Name != "ManualImport") {
		err = errors.New("manual import command identity changed")
	}
	return command, err
}

func (c *Client) RemoveImportedQueueItem(ctx context.Context, id int) error {
	if id < 1 {
		return errors.New("invalid imported queue ID")
	}
	return c.do(ctx, http.MethodDelete, c.apiPath("queue", strconv.Itoa(id)), url.Values{"removeFromClient": {"true"}, "blocklist": {"false"}, "skipRedownload": {"true"}}, nil, nil)
}

func MatchedIDImportReason(kind, value string) bool {
	entity := "movie"
	if kind == "sonarr" {
		entity = "series"
	} else if kind != "radarr" {
		return false
	}
	prefix := "found matching " + entity + " via grab history, but release was matched to " + entity + " by id"
	value = strings.ToLower(strings.TrimSpace(value))
	return value == prefix || strings.HasPrefix(value, prefix+".")
}

// Queue titles can be filenames; messages contain the concrete rejections.
// An ID warning never grants permission to bypass an additional rejection.
func (q QueueRecord) AllowsMatchedIDImport(kind string) bool {
	if !q.NeedsImportRecovery() {
		return false
	}
	found := false
	for _, status := range q.StatusMessages {
		if MatchedIDImportReason(kind, status.Title) {
			found = true
		} else if containsRecoverableImportText(status.Title) {
			return false
		}
		for _, message := range status.Messages {
			if MatchedIDImportReason(kind, message) {
				found = true
			} else if strings.TrimSpace(message) != "" {
				return false
			}
		}
		if len(status.Messages) == 0 && !MatchedIDImportReason(kind, status.Title) && !containsImportBlockedText(status.Title) && strings.TrimSpace(status.Title) != "" {
			return false
		}
	}
	return found
}
