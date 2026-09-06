package main

import (
	"encoding/json"
	"os"
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
	ID   int    `json:"id"`
	Year int    `json:"year"`
	Path string `json:"path"`
}

type Movie struct {
	ID         int    `json:"id"`
	Year       int    `json:"year"`
	Path       string `json:"path"`
	FolderPath string `json:"folderPath"`
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

func (q QueueRecord) needsImportRecovery() bool {
	if !strings.EqualFold(strings.TrimSpace(q.Status), "completed") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(q.TrackedDownloadState), "importBlocked") {
		return true
	}
	for _, status := range q.StatusMessages {
		if containsImportBlockedText(status.Title) {
			return true
		}
		for _, message := range status.Messages {
			if containsImportBlockedText(message) {
				return true
			}
		}
	}
	return false
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
	ID            int        `json:"id"`
	SeriesID      int        `json:"seriesId"`
	EpisodeFileID int        `json:"episodeFileId"`
	AirDate       string     `json:"airDate"`
	AirDateUTC    *time.Time `json:"airDateUtc"`
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

type ProbeResult struct {
	Streams []ProbeStream `json:"streams"`
}

type ProbeStream struct {
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

type Validation struct {
	fileInfo           os.FileInfo
	dirInfo            os.FileInfo
	Valid              bool     `json:"valid"`
	HasSubtitles       bool     `json:"hasSubtitles"`
	HasEnglish         bool     `json:"hasEnglishSubtitles"`
	HasUnknownLanguage bool     `json:"hasUnknownSubtitleLanguage"`
	SubtitleLangs      []string `json:"subtitleLanguages,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

type State struct {
	Attempts   map[string]int           `json:"attempts"`
	Operations map[string]Operation     `json:"operations,omitempty"`
	Completed  map[string]bool          `json:"completed,omitempty"`
	Instances  map[string]string        `json:"instances,omitempty"`
	Webhooks   map[string]StoredWebhook `json:"webhooks,omitempty"`
}

type StoredWebhook struct {
	Kind        string         `json:"kind"`
	Payload     WebhookPayload `json:"payload"`
	Failures    int            `json:"failures"`
	NextAttempt time.Time      `json:"nextAttempt"`
}

// A phase is persisted BEFORE its network mutation. An unfinished operation is
// a safety latch, not permission to replay a non-idempotent request after restart.
type Operation struct {
	Kind       string `json:"kind"`
	SubjectID  int    `json:"subjectId"`
	FileID     int    `json:"fileId,omitempty"`
	DownloadID string `json:"downloadId,omitempty"`
	EpisodeIDs []int  `json:"episodeIds,omitempty"`
	Phase      string `json:"phase"`
}

type UnmatchedReport struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Roots       []string         `json:"roots"`
	Files       []UnmatchedMedia `json:"files"`
}

type UnmatchedMedia struct {
	Path string `json:"path"`
}
