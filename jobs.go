package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"
)

func storedWebhook(kind string, payload WebhookPayload) (string, StoredWebhook, error) {
	if kind != "sonarr" && kind != "radarr" {
		return "", StoredWebhook{}, errors.New("unknown Arr kind")
	}
	ids := []int{}
	for _, file := range payload.files(kind) {
		if file.ID < 1 {
			return "", StoredWebhook{}, errors.New("webhook file IDs must be positive")
		}
		ids = append(ids, file.ID)
	}
	if len(ids) == 0 {
		return "", StoredWebhook{}, errors.New("webhook contains no media files")
	}
	minimal := WebhookPayload{EventType: "Download"}
	// Keep the origin only as a consistency hint. Processing must resolve the
	// same identity from Arr history before it can be used for remediation.
	minimal.DownloadID = payload.DownloadID
	for _, id := range canonicalIDs(ids) {
		if kind == "sonarr" {
			minimal.EpisodeFiles = append(minimal.EpisodeFiles, WebhookFile{ID: id})
		} else {
			minimal.MovieFiles = append(minimal.MovieFiles, WebhookFile{ID: id})
		}
	}
	if payload.Series != nil {
		minimal.Series = &Series{ID: payload.Series.ID}
	}
	if payload.Movie != nil {
		minimal.Movie = &Movie{ID: payload.Movie.ID}
	}
	data, err := json.Marshal(minimal)
	if err != nil {
		return "", StoredWebhook{}, err
	}
	return fmt.Sprintf("%s:%x", kind, sha256.Sum256(data)), StoredWebhook{Kind: kind, Payload: minimal}, nil
}

func (s *StateStore) AddWebhook(key string, job StoredWebhook) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.state.Webhooks[key]; exists {
		if !existing.NeedsReview {
			return nil
		}
		// Redelivery asks for a fresh evaluation, never replay of journaled work.
		return s.updateLocked(func(next *State) {
			existing.NeedsReview = false
			existing.Failures = 0
			existing.NextAttempt = time.Time{}
			next.Webhooks[key] = existing
		})
	}
	if len(s.state.Webhooks) >= 1000 {
		return errors.New("durable webhook queue is full")
	}
	return s.updateLocked(func(next *State) { next.Webhooks[key] = job })
}

func (s *StateStore) Webhooks() map[string]StoredWebhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.state.Webhooks)
}

func (s *StateStore) FinishWebhook(key string, processingErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Webhooks[key]
	if !ok {
		return errors.New("durable webhook entry is missing")
	}
	return s.updateLocked(func(next *State) {
		if processingErr == nil {
			delete(next.Webhooks, key)
			return
		}
		job.Failures = min(job.Failures+1, maxWebhookBackoffFailures)
		job.NeedsReview = !canRetryProcessing(processingErr)
		job.NextAttempt = time.Now().Add(webhookBackoff(job.Failures))
		next.Webhooks[key] = job
	})
}

func (s *Service) dispatchStoredJobs() {
	if s.config.DryRun || s.state == nil {
		return
	}
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	if s.stopping {
		return
	}
	if s.scheduled == nil {
		s.scheduled = map[string]bool{}
	}
	for key, stored := range s.state.Webhooks() {
		if s.scheduled[key] || stored.NeedsReview || time.Now().Before(stored.NextAttempt) {
			continue
		}
		client := s.arr[stored.Kind]
		if client == nil {
			continue
		}
		select {
		case s.jobs <- webhookJob{key: key, client: client, payload: stored.Payload}:
			s.scheduled[key] = true
		default:
			return
		}
	}
}

func (s *Service) finishJob(job webhookJob, err error) {
	if job.key == "" {
		return
	}
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	// On shutdown preserve the job without consuming a retry attempt.
	if !s.stopping && !errors.Is(err, context.Canceled) {
		if saveErr := s.state.FinishWebhook(job.key, err); saveErr != nil {
			s.log.Error("could not persist webhook result", "error", saveErr)
		} else if err != nil && !canRetryProcessing(err) {
			s.log.Warn("webhook paused for review; redelivery can recheck without replaying mutations", "arr", job.client.Kind(), "error", err)
		}
	}
	delete(s.scheduled, job.key)
}
