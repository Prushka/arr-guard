package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
	"github.com/Prushka/arr-guard/internal/config"
	"github.com/Prushka/arr-guard/internal/probe"
)

type Service struct {
	config       config.Config
	log          *slog.Logger
	probe        probe.Prober
	probeFn      func(context.Context, string) (probe.Validation, error)
	state        *StateStore
	arr          map[string]*arr.Client
	jobs         chan webhookJob
	stop         chan struct{}
	wg           sync.WaitGroup
	locksMu      sync.Mutex
	locks        map[string]*fileLock
	handledMu    sync.Mutex
	handled      map[string]time.Time
	stopOnce     sync.Once
	mutationMu   sync.Mutex
	workerCancel context.CancelFunc
	enqueueMu    sync.Mutex
	stopping     bool
	scheduled    map[string]bool
}

// fileLock tracks waiters so an idle lock can be removed without allowing
// another webhook to create a second lock for the same media file.
type fileLock struct {
	mu   sync.Mutex
	refs int
}

type webhookJob struct {
	key     string
	client  *arr.Client
	payload arr.WebhookPayload
}

const blockedQueueScanInterval = time.Hour

const silentMediaSkipAfterYears = 50

// Keep successful webhook IDs briefly to absorb Arr retries while bounding
// memory use. Failed processing removes the claim immediately for retry.
const webhookDedupTTL = 24 * time.Hour

func NewService(config config.Config, log *slog.Logger) (*Service, error) {
	state, err := openServiceState(config)
	if err != nil {
		return nil, err
	}
	service := &Service{
		config:  config,
		log:     log,
		probe:   probe.Prober{Path: config.FFprobePath, Timeout: 10 * time.Minute},
		state:   state,
		arr:     make(map[string]*arr.Client),
		jobs:    make(chan webhookJob, config.Workers*4),
		stop:    make(chan struct{}),
		locks:   make(map[string]*fileLock),
		handled: make(map[string]time.Time),
	}
	if config.Sonarr != nil {
		service.arr["sonarr"] = arr.NewClient(*config.Sonarr, log)
	}
	if config.Radarr != nil {
		service.arr["radarr"] = arr.NewClient(*config.Radarr, log)
	}
	for _, client := range service.arr {
		if config.DryRun || config.Mode == "unmatched" {
			client.EnforceReadOnly()
		}
	}
	if !config.DryRun && config.Mode != "unmatched" {
		if err := state.Bind(service.arr); err != nil {
			_ = state.Close()
			return nil, err
		}
	}
	return service, nil
}

func (s *Service) StartWorkers(ctx context.Context) {
	ctx, s.workerCancel = context.WithCancel(ctx)
	for i := 0; i < s.config.Workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case item := <-s.jobs:
					jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
					err := s.processWebhook(jobCtx, item.client, item.payload)
					if err != nil && !errors.Is(err, context.Canceled) {
						s.log.Error("webhook processing failed", "error", err, "arr", item.client.Kind())
					}
					cancel()
					s.finishJob(item, err)
				case <-s.stop:
					return
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			s.dispatchStoredJobs()
			_ = s.reconcileManualImports(ctx, nil)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runBlockedQueueScan(ctx)

		ticker := time.NewTicker(blockedQueueScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanupWebhookState()
				s.runBlockedQueueScan(ctx)
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Service) StopWorkers() {
	s.enqueueMu.Lock()
	s.stopping = true
	s.enqueueMu.Unlock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// CleanupPending verifies pending imports and reports unfinished operations
// without replaying mutations. Verified imports may begin separately journaled
// residual cleanup. Arr has no idempotency keys, so uncertain writes stay protected.
func (s *Service) CleanupPending(ctx context.Context) error {
	if s.config.DryRun || s.config.Mode == "unmatched" || s.state == nil {
		return nil
	}
	_ = s.reconcileManualImports(ctx, nil)
	pending := s.state.Pending()
	if len(pending) == 0 {
		return nil
	}
	for key, op := range pending {
		s.log.Error("unfinished operation requires reconciliation", "key", key, "phase", op.Phase)
	}
	return fmt.Errorf("%d unfinished operations retained in STATE_PATH; automatic replay disabled", len(pending))
}

func (s *Service) runBlockedQueueScan(ctx context.Context) {
	if !s.config.RecoverBlockedQueue {
		return
	}
	for _, client := range s.arr {
		if err := s.recoverBlockedQueue(ctx, client); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("blocked queue scan failed", "arr", client.Kind(), "error", err)
		}
	}
}

func (s *Service) recoverBlockedQueue(ctx context.Context, client *arr.Client) error {
	firstErr := s.reconcileManualImports(ctx, client)
	queue, err := client.Queue(ctx)
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}

	recovered := 0
	searched := map[string]bool{}
	seen := map[string]bool{}
	for _, item := range queue {
		if !item.NeedsImportRecovery() {
			continue
		}
		key := strings.ToLower(item.DownloadID)
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := s.recoverQueueItem(ctx, client, item, searched); err != nil {
			s.log.Error("blocked queue item recovery failed", "arr", client.Kind(), "queue_id", item.ID, "download_id", item.DownloadID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recovered++
	}
	s.log.Info("blocked queue scan complete", "arr", client.Kind(), "queue_items", len(queue), "recovered", recovered)
	return firstErr
}

func (s *Service) Enqueue(client *arr.Client, payload arr.WebhookPayload) error {
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	if s.stopping {
		return errors.New("service is stopping")
	}
	if !s.config.DryRun && s.state != nil {
		key, stored, err := storedWebhook(client.Kind(), payload)
		if err != nil {
			return err
		}
		return s.state.AddWebhook(key, stored)
	}
	select {
	case s.jobs <- webhookJob{client: client, payload: payload}:
		return nil
	default:
		return errors.New("webhook queue is full")
	}
}

func (s *Service) processWebhook(ctx context.Context, client *arr.Client, payload arr.WebhookPayload) error {
	if !strings.EqualFold(payload.EventType, "download") && !strings.EqualFold(payload.EventType, "importcomplete") {
		return nil
	}
	files := payload.Files(client.Kind())
	if len(files) == 0 {
		return errors.New("download webhook contains no media file")
	}
	var errs []error
	for _, wf := range files {
		if wf.ID < 1 {
			errs = append(errs, errors.New("webhook media ID must be positive"))
			continue
		}
		key := operationKey(client.Kind(), wf.ID)
		lock := s.acquireFileLock(key)
		if !s.claimWebhook(key) {
			s.releaseFileLock(key, lock)
			continue
		}
		err := func() error {
			file, err := client.GetMediaFile(ctx, wf.ID)
			if err != nil {
				var apiErr *arr.HTTPError
				if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
					return nil
				}
				return err
			}
			if client.Kind() == "sonarr" {
				if payload.Series != nil && payload.Series.ID != file.ParentID {
					return errors.New("webhook series does not own file")
				}
				file.Year, err = client.EpisodeReleaseYearForFile(ctx, file.ParentID, file.ID)
			} else {
				if payload.Movie != nil && payload.Movie.ID != file.MovieID {
					return errors.New("webhook movie does not own file")
				}
				var movie arr.Movie
				movie, err = client.GetMovie(ctx, file.MovieID)
				if err == nil && movie.ID != file.MovieID {
					return errors.New("movie metadata identity mismatch")
				}
				file.Year = movie.Year
			}
			if err != nil {
				return err
			}
			// Paths, release dates, episodes and origin are always resolved from Arr.
			return s.auditFileWithOrigin(ctx, client, file, payload.DownloadID)
		}()
		s.finishWebhook(key, err)
		s.releaseFileLock(key, lock)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) claimWebhook(key string) bool {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	if s.handled == nil {
		s.handled = make(map[string]time.Time)
	}
	now := time.Now()
	s.cleanupWebhookStateLocked(now)
	if handledAt, ok := s.handled[key]; ok && now.Sub(handledAt) < webhookDedupTTL {
		return false
	}
	s.handled[key] = now
	return true
}

func (s *Service) cleanupWebhookState() {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	s.cleanupWebhookStateLocked(time.Now())
}

func (s *Service) cleanupWebhookStateLocked(now time.Time) {
	for handledKey, handledAt := range s.handled {
		if now.Sub(handledAt) >= webhookDedupTTL {
			delete(s.handled, handledKey)
		}
	}
}

func (s *Service) finishWebhook(key string, err error) {
	s.handledMu.Lock()
	defer s.handledMu.Unlock()
	if err != nil {
		delete(s.handled, key)
		return
	}
	if s.handled == nil {
		s.handled = make(map[string]time.Time)
	}
	s.handled[key] = time.Now()
}

func (s *Service) acquireFileLock(key string) *fileLock {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*fileLock)
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &fileLock{}
		s.locks[key] = lock
	}
	lock.refs++
	s.locksMu.Unlock()
	lock.mu.Lock()
	return lock
}

func (s *Service) releaseFileLock(key string, lock *fileLock) {
	lock.mu.Unlock()
	s.locksMu.Lock()
	lock.refs--
	if lock.refs == 0 && s.locks[key] == lock {
		delete(s.locks, key)
	}
	s.locksMu.Unlock()
}
