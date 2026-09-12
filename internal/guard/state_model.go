package guard

import (
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
)

type State struct {
	Attempts   map[string]int           `json:"attempts"`
	Operations map[string]Operation     `json:"operations,omitempty"`
	Completed  map[string]bool          `json:"completed,omitempty"`
	Instances  map[string]string        `json:"instances,omitempty"`
	Webhooks   map[string]StoredWebhook `json:"webhooks,omitempty"`
}

type StoredWebhook struct {
	Kind        string             `json:"kind"`
	Payload     arr.WebhookPayload `json:"payload"`
	Failures    int                `json:"failures"`
	NextAttempt time.Time          `json:"nextAttempt"`
	NeedsReview bool               `json:"needsReview,omitempty"`
}

// A phase is persisted BEFORE its network mutation. An unfinished operation is
// a safety latch, not permission to replay a non-idempotent request after restart.
type Operation struct {
	Kind             string `json:"kind"`
	SubjectID        int    `json:"subjectId"`
	FileID           int    `json:"fileId,omitempty"`
	DownloadID       string `json:"downloadId,omitempty"`
	EpisodeIDs       []int  `json:"episodeIds,omitempty"`
	Phase            string `json:"phase"`
	AutomaticSearch  bool   `json:"automaticSearch,omitempty"`
	SearchEpisodeIDs []int  `json:"searchEpisodeIds,omitempty"`
}
