package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Only errors known to precede mutations may authorize automatic fresh retries.
// A mutation's network error must never inherit retryability from its cause.
type deferredProcessingError struct{ error }

func (e *deferredProcessingError) Unwrap() error { return e.error }
func deferProcessing(err error) error            { return &deferredProcessingError{err} }

type reconciliationError struct{ error }

func (e *reconciliationError) Unwrap() error { return e.error }
func requireReconciliation(err error) error  { return &reconciliationError{err} }

// A file can disappear between a probe and its preflight. A fresh job/scan read
// can then confirm its absence, without treating unrelated endpoint 404s as safe.
func deferMissingMedia(err error) error {
	var apiErr *ArrHTTPError
	if errors.As(err, &apiErr) && apiErr.Method == http.MethodGet && apiErr.Status == http.StatusNotFound {
		return deferProcessing(err)
	}
	return err
}

func canRetryProcessing(err error) bool {
	if err == nil {
		return false
	}
	// Each file in a batch has already been processed independently. Keep a batch
	// alive if another file can recover, without replaying its journaled mutations.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if canRetryProcessing(child) {
				return true
			}
		}
		return false
	}
	var uncertain *reconciliationError
	if errors.As(err, &uncertain) {
		return false
	}
	var deferred *deferredProcessingError
	if errors.As(err, &deferred) {
		return true
	}
	var apiErr *ArrHTTPError
	if errors.As(err, &apiErr) {
		return apiErr.Method == "GET" && (apiErr.Status >= 500 || apiErr.Status == 408 || apiErr.Status == 409 || apiErr.Status == 429)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	return errors.As(err, &network)
}

const scanReadAttempts = 3
const maxWebhookBackoffFailures = 7

func webhookBackoff(failures int) time.Duration {
	return min(time.Minute*time.Duration(1<<max(0, min(failures-1, 6))), time.Hour)
}

// A one-time scan stays finite. Retries always fetch new resources/probe results.
func retryScanReads(ctx context.Context, run func() error) error {
	for attempt := 0; ; attempt++ {
		err := run()
		if err == nil || !canRetryProcessing(err) || attempt+1 >= scanReadAttempts || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) auditFileWithRetries(ctx context.Context, client *ArrClient, file MediaFile) error {
	payload := WebhookPayload{EventType: "Download"}
	if client.Kind() == "sonarr" {
		payload.EpisodeFile = &WebhookFile{ID: file.ID}
	} else {
		payload.MovieFile = &WebhookFile{ID: file.ID}
	}
	first := true
	err := retryScanReads(ctx, func() error {
		if first {
			first = false
			return s.auditFile(ctx, client, file)
		}
		// Scan retries bypass webhook delivery deduplication, but still honor the
		// operation journal and always refresh authoritative metadata.
		fresh, readErr := client.GetMediaFile(ctx, file.ID)
		if readErr != nil {
			var apiErr *ArrHTTPError
			if errors.As(readErr, &apiErr) && apiErr.Status == http.StatusNotFound {
				return nil
			}
			return readErr
		}
		if client.Kind() == "sonarr" {
			fresh.Year, readErr = client.EpisodeReleaseYearForFile(ctx, fresh.ParentID, fresh.ID)
		} else {
			var movie Movie
			readErr = client.do(ctx, http.MethodGet, client.apiPath("movie", strconv.Itoa(fresh.MovieID)), nil, nil, &movie)
			if readErr == nil && movie.ID != fresh.MovieID {
				return errors.New("movie metadata identity mismatch")
			}
			fresh.Year = movie.Year
		}
		if readErr != nil {
			return readErr
		}
		return s.auditFile(ctx, client, fresh)
	})
	if err != nil && canRetryProcessing(err) && ctx.Err() == nil && !s.config.DryRun {
		if queueErr := s.Enqueue(client, payload); queueErr != nil {
			return errors.Join(err, queueErr)
		}
		s.log.Info("deferred scan file saved for serve retry", "arr", client.Kind(), "file_id", file.ID)
	}
	return err
}
