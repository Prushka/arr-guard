package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/probe"
)

// Called only by the opt-in GET-only diagnostic harness after real probes and
// normal scan/webhook checks. Simulate exhaustion and rejection in isolated test
// memory to verify the cap against current authoritative Arr ownership. This is
// not a claim that the selected live file actually lacks English subtitles.
func testLiveRemediationLimit(t *testing.T, original *Service, client *arr.Client, file arr.MediaFile, validation probe.Validation, localPath string) {
	t.Helper()
	if !original.config.DryRun || !client.IsReadOnly() {
		t.Fatal("live remediation-limit check requires dry run and read-only client")
	}
	var ids []int
	if client.Kind() == "sonarr" {
		var err error
		ids, err = client.EpisodeIDsForFile(t.Context(), file.ParentID, file.ID)
		if err != nil || len(ids) == 0 {
			t.Fatal("live remediation-limit episode mapping unavailable")
		}
	}
	var logs bytes.Buffer
	s := &Service{config: original.config, log: slog.New(slog.NewTextHandler(&logs, nil)), state: &StateStore{state: State{Attempts: map[string]int{}}}}
	for _, key := range retryKeys(client.Kind(), file, ids) {
		s.state.state.Attempts[key] = s.config.MaxAttempts
	}
	validation.Valid, validation.HasUnknownLanguage = false, false
	validation.Reason = "test-only simulated subtitle rejection"
	s.probeFn = func(context.Context, string) (probe.Validation, error) { return validation, nil }
	before, err := json.Marshal(s.state.state)
	if err != nil {
		t.Fatal("cannot snapshot simulated retry state")
	}
	if err := s.applyValidation(t.Context(), client, file, validation, localPath); err != nil {
		t.Fatalf("live remediation-limit scan check: %s", liveErrorCategory(err))
	}
	payload := arr.WebhookPayload{EventType: "Download", EpisodeFile: &arr.WebhookFile{ID: file.ID}, MovieFile: &arr.WebhookFile{ID: file.ID}}
	if err := s.processWebhook(t.Context(), client, payload); err != nil {
		t.Fatalf("live remediation-limit webhook check: %s", liveErrorCategory(err))
	}
	after, err := json.Marshal(s.state.state)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("live remediation-limit check changed simulated retry state")
	}
	if strings.Count(logs.String(), "subtitle remediation skipped; attempt limit reached") != 2 {
		t.Fatal("scan and webhook did not both skip exhausted remediation")
	}
	t.Log("simulated_exhausted_budget=true simulated_rejection=true live_ownership=true scan=skipped webhook=skipped mutations=0 retry_writes=0 media_writes=0")
}
