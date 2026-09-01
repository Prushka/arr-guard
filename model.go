package main

import (
	"encoding/json"
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
	ID          int               `json:"id"`
	DownloadID  string            `json:"downloadId"`
	EventType   string            `json:"eventType"`
	SourceTitle string            `json:"sourceTitle"`
	Data        map[string]string `json:"data"`
}

type HistoryPage struct {
	Records []HistoryRecord `json:"records"`
}

type QueueRecord struct {
	ID         int    `json:"id"`
	DownloadID string `json:"downloadId"`
}

type QueuePage struct {
	Records []QueueRecord `json:"records"`
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
	ID            int `json:"id"`
	EpisodeFileID int `json:"episodeFileId"`
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
	Valid              bool     `json:"valid"`
	HasSubtitles       bool     `json:"hasSubtitles"`
	HasEnglish         bool     `json:"hasEnglishSubtitles"`
	HasUnknownLanguage bool     `json:"hasUnknownSubtitleLanguage"`
	SubtitleLangs      []string `json:"subtitleLanguages,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

type State struct {
	Attempts map[string]int `json:"attempts"`
}
