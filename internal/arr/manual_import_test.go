package arr

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Prushka/arr-guard/internal/config"
)

func TestMatchedIDImportReasonRequiresExactFamily(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		entity := "movie"
		if kind == "sonarr" {
			entity = "series"
		}
		reason := "Found matching " + entity + " via grab history, but release was matched to " + entity + " by ID"
		q := QueueRecord{Status: "completed", TrackedDownloadState: "importBlocked", StatusMessages: []QueueStatusMessage{{Title: "Unable to import automatically", Messages: []string{strings.ToUpper(reason) + ". Manual Import required."}}}}
		if !q.AllowsMatchedIDImport(kind) {
			t.Fatal("supported warning not recognized")
		}
		q.StatusMessages[0].Messages = append(q.StatusMessages[0].Messages, "Unable to parse file")
		if q.AllowsMatchedIDImport(kind) {
			t.Fatal("mixed warning was allowed")
		}
		q.StatusMessages = []QueueStatusMessage{{Title: reason + "entification"}}
		if q.AllowsMatchedIDImport(kind) {
			t.Fatal("prefix collision was allowed")
		}
		q.StatusMessages = []QueueStatusMessage{{Title: reason}}
		if !q.AllowsMatchedIDImport(kind) {
			t.Fatal("title warning not recognized")
		}
		for _, state := range []string{"importing", "imported", "failedPending", "unknown"} {
			q.TrackedDownloadState = state
			if q.AllowsMatchedIDImport(kind) {
				t.Fatal("active or unknown state was allowed")
			}
		}
	}
}

func TestReadOnlyClientRejectsManualImportAndCleanup(t *testing.T) {
	c := NewClient(config.Arr{Kind: "radarr", URL: "http://127.0.0.1:1", APIVersion: "v3"}, slog.Default())
	c.EnforceReadOnly()
	_, err := c.ManualImport(context.Background(), []ManualImportFile{{Path: "fixture.mkv", MovieID: 3}})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatal("manual import escaped read-only gate")
	}
	if err := c.RemoveImportedQueueItem(context.Background(), 7); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatal("cleanup escaped read-only gate")
	}
}
