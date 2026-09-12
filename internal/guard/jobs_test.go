package guard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/probe"
)

func TestDurableWebhookAdmissionSurvivesRestartAndRetries(t *testing.T) {
	f := newSafetyFixture(t, "sonarr")
	payload := arr.WebhookPayload{EventType: "Download", EpisodeFiles: []arr.WebhookFile{{ID: 17}, {ID: 17}}, DownloadID: "do-not-trust", Episodes: []arr.Episode{{ID: 999}}}
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadStateStore(f.service.state.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Webhooks()) != 1 {
		t.Fatal("accepted webhook was not persisted or was duplicated")
	}
	f.service.state = reloaded
	f.service.jobs = make(chan webhookJob, 1)
	f.service.dispatchStoredJobs()
	job := <-f.service.jobs
	if job.payload.DownloadID != payload.DownloadID || len(job.payload.Episodes) != 0 || len(job.payload.Files("sonarr")) != 1 {
		t.Fatal("persisted unexpected webhook metadata")
	}
	f.service.finishJob(job, deferProcessing(errors.New("read failed")))
	f.service.dispatchStoredJobs()
	if len(f.service.jobs) != 0 {
		t.Fatal("failed webhook ignored backoff")
	}
	for i := 1; i < 12; i++ {
		if err := reloaded.FinishWebhook(job.key, deferProcessing(errors.New("read failed"))); err != nil {
			t.Fatal(err)
		}
	}
	f.service.dispatchStoredJobs()
	if len(f.service.jobs) != 0 {
		t.Fatal("failed webhook ignored capped backoff")
	}
	if reloaded.Webhooks()[job.key].NeedsReview {
		t.Fatal("transient job was permanently paused")
	}
	if err := reloaded.FinishWebhook(job.key, nil); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Webhooks()) != 0 {
		t.Fatal("successful webhook not removed")
	}
}

func TestWebhookNeverAcknowledgesFailedPersistence(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.service.config.WebhookToken = "test"
	f.service.state.path = t.TempDir()
	req := httptest.NewRequest(http.MethodPost, "/webhook/radarr", bytes.NewBufferString(`{"eventType":"Download","movieFile":{"id":17}}`))
	req.Header.Set("X-Webhook-Token", "test")
	response := httptest.NewRecorder()
	f.service.WebhookHandler("radarr")(response, req)
	if response.Code != http.StatusServiceUnavailable || len(f.service.state.Webhooks()) != 0 {
		t.Fatal("acknowledged a lost webhook")
	}
}

func TestWorkersStopPreservesAcceptedJobsAndRejectsEnqueue(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	f.service.jobs = make(chan webhookJob, 4)
	f.service.stop = make(chan struct{})
	entered := make(chan struct{})
	f.service.probeFn = func(ctx context.Context, _ string) (probe.Validation, error) {
		close(entered)
		<-ctx.Done()
		return probe.Validation{}, ctx.Err()
	}
	payload := arr.WebhookPayload{EventType: "Download", MovieFile: &arr.WebhookFile{ID: 17}}
	if err := f.service.Enqueue(f.client, payload); err != nil {
		t.Fatal(err)
	}
	f.service.StartWorkers(t.Context())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	done := make(chan struct{})
	go func() { f.service.StopWorkers(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker shutdown hung")
	}
	if err := f.service.Enqueue(f.client, payload); err == nil {
		t.Fatal("accepted webhook after shutdown")
	}
	jobs := f.service.state.Webhooks()
	if len(jobs) != 1 {
		t.Fatal("shutdown lost accepted work")
	}
	for _, job := range jobs {
		if job.Failures != 0 {
			t.Fatal("shutdown consumed a retry")
		}
	}
	if len(f.mutations) != 0 {
		t.Fatal("shutdown mutated Arr")
	}
}

func TestWebhookPayloadValidationAndTestEvents(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"eventType":"Test"}`, 204},
		{`{"eventType":"Grab"}`, 204},
		{`{"eventType":"Download"}`, 400},
		{`{"eventType":"Download","movieFile":{"id":-1}}`, 400},
		{`{"eventType":"Test"} {}`, 400},
	} {
		r := httptest.NewRequest(http.MethodPost, "/webhook/radarr", bytes.NewBufferString(test.body))
		w := httptest.NewRecorder()
		f.service.WebhookHandler("radarr")(w, r)
		if w.Code != test.status {
			t.Errorf("status=%d want=%d", w.Code, test.status)
		}
	}
	if _, err := os.Stat(f.service.state.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ignored webhook wrote state")
	}
}

func TestCanceledWorkerPreservesJobBeforeStopFlagIsSet(t *testing.T) {
	f := newSafetyFixture(t, "radarr")
	if err := f.service.Enqueue(f.client, arr.WebhookPayload{EventType: "Download", MovieFile: &arr.WebhookFile{ID: 17}}); err != nil {
		t.Fatal(err)
	}
	f.service.jobs = make(chan webhookJob, 1)
	f.service.dispatchStoredJobs()
	job := <-f.service.jobs
	f.service.finishJob(job, context.Canceled)
	stored, ok := f.service.state.Webhooks()[job.key]
	if !ok || stored.Failures != 0 {
		t.Fatal("context cancellation lost job or consumed retry")
	}
}
