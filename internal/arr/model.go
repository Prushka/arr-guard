package arr

import (
	"encoding/json"
	"strings"
	"time"
)

type MediaFile struct {
	ID           int        `json:"id"`
	ParentID     int        `json:"seriesId,omitempty"`
	MovieID      int        `json:"movieId,omitempty"`
	Year         int        `json:"year,omitempty"`
	RelativePath string     `json:"relativePath"`
	Path         string     `json:"path"`
	Size         int64      `json:"size"`
	DateAdded    *time.Time `json:"dateAdded,omitempty"`
}

func (f MediaFile) SubjectID(kind string) int {
	if kind == "radarr" {
		return f.MovieID
	}
	return f.ParentID
}

type Series struct {
	ID              int              `json:"id"`
	Year            int              `json:"year"`
	Path            string           `json:"path"`
	Title           string           `json:"title,omitempty"`
	AlternateTitles []AlternateTitle `json:"alternateTitles,omitempty"`
}

type Movie struct {
	ID              int              `json:"id"`
	Year            int              `json:"year"`
	Path            string           `json:"path"`
	FolderPath      string           `json:"folderPath"`
	Title           string           `json:"title,omitempty"`
	OriginalTitle   string           `json:"originalTitle,omitempty"`
	AlternateTitles []AlternateTitle `json:"alternateTitles,omitempty"`
}

type AlternateTitle struct {
	Title        string `json:"title"`
	SeasonNumber int    `json:"seasonNumber"`
}

type HistoryRecord struct {
	MovieID     int               `json:"movieId"`
	SeriesID    int               `json:"seriesId"`
	EpisodeID   int               `json:"episodeId"`
	ID          int               `json:"id"`
	DownloadID  string            `json:"downloadId"`
	EventType   string            `json:"eventType"`
	SourceTitle string            `json:"sourceTitle"`
	Data        map[string]string `json:"data"`
}

type HistoryPage struct {
	Records      []HistoryRecord `json:"records"`
	TotalRecords int             `json:"totalRecords"`
}

type QueueRecord struct {
	ID                    int                  `json:"id"`
	DownloadID            string               `json:"downloadId"`
	OutputPath            string               `json:"outputPath"`
	MovieID               int                  `json:"movieId"`
	SeriesID              int                  `json:"seriesId"`
	EpisodeID             int                  `json:"episodeId"`
	Status                string               `json:"status"`
	TrackedDownloadStatus string               `json:"trackedDownloadStatus"`
	TrackedDownloadState  string               `json:"trackedDownloadState"`
	StatusMessages        []QueueStatusMessage `json:"statusMessages"`
}

type QueueStatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

func (q QueueRecord) NeedsImportRecovery() bool {
	if !strings.EqualFold(strings.TrimSpace(q.Status), "completed") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(q.TrackedDownloadState), "importBlocked") {
		return true
	}
	state := strings.ToLower(strings.TrimSpace(q.TrackedDownloadState))
	if state != "" && state != "importpending" {
		return false
	}
	for _, status := range q.StatusMessages {
		if containsImportBlockedText(status.Title) || containsRecoverableImportText(status.Title) {
			return true
		}
		for _, message := range status.Messages {
			if containsImportBlockedText(message) || containsRecoverableImportText(message) {
				return true
			}
		}
	}
	return false
}

func containsRecoverableImportText(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{
		"not an upgrade for existing episode",
		"not an upgrade for existing movie",
		"no files found are eligible for import",
		"not a quality revision upgrade for existing episode",
		"not a quality revision upgrade for existing movie",
		"unable to parse file",
		"unable to determine if file is a sample",
		"found matching movie via grab history, but release was matched to movie by id",
		"found matching series via grab history, but release was matched to series by id",
		"caution: found executable file",
	} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return strings.Contains(value, " was unexpected considering the ")
}

func containsImportBlockedText(value string) bool {
	return strings.Contains(strings.ToLower(value), "unable to import automatically")
}

type QueuePage struct {
	Records      []QueueRecord `json:"records"`
	TotalRecords int           `json:"totalRecords"`
}

type CommandRequest struct {
	Name       string `json:"name,omitempty"`
	SeriesID   int    `json:"seriesId,omitempty"`
	MovieIDs   []int  `json:"movieIds,omitempty"`
	EpisodeIDs []int  `json:"episodeIds,omitempty"`
}

type WebhookPayload struct {
	EventType      string          `json:"eventType"`
	DownloadID     string          `json:"downloadId"`
	InstanceName   string          `json:"instanceName"`
	ApplicationURL string          `json:"applicationUrl"`
	Series         *Series         `json:"series"`
	Movie          *Movie          `json:"movie"`
	Episodes       []Episode       `json:"episodes"`
	EpisodeFile    *WebhookFile    `json:"episodeFile"`
	MovieFile      *WebhookFile    `json:"movieFile"`
	EpisodeFiles   []WebhookFile   `json:"episodeFiles"`
	MovieFiles     []WebhookFile   `json:"movieFiles"`
	DeletedFiles   []WebhookFile   `json:"deletedFiles"`
	Release        json.RawMessage `json:"release"`
}

type Episode struct {
	ID                         int        `json:"id"`
	SeriesID                   int        `json:"seriesId"`
	EpisodeFileID              int        `json:"episodeFileId"`
	AirDate                    string     `json:"airDate"`
	AirDateUTC                 *time.Time `json:"airDateUtc"`
	SeasonNumber               *int       `json:"seasonNumber,omitempty"`
	EpisodeNumber              *int       `json:"episodeNumber,omitempty"`
	AbsoluteEpisodeNumber      *int       `json:"absoluteEpisodeNumber,omitempty"`
	SceneSeasonNumber          *int       `json:"sceneSeasonNumber,omitempty"`
	SceneEpisodeNumber         *int       `json:"sceneEpisodeNumber,omitempty"`
	SceneAbsoluteEpisodeNumber *int       `json:"sceneAbsoluteEpisodeNumber,omitempty"`
}

func (e Episode) ReleaseYear() int {
	if date, err := time.Parse("2006-01-02", e.AirDate); err == nil && date.Year() > 0 {
		return date.Year()
	}
	if e.AirDateUTC != nil {
		return e.AirDateUTC.Year()
	}
	return 0
}

type WebhookFile struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	MovieID      int    `json:"movieId"`
	RelativePath string `json:"relativePath"`
	Path         string `json:"path"`
	SourcePath   string `json:"sourcePath"`
}
