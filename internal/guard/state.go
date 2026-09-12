package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type StateStore struct {
	path     string
	mu       sync.Mutex
	state    State
	lockFile *os.File
	writeErr error
}

func LoadStateStore(path string) (*StateStore, error) {
	store := &StateStore{path: path, state: State{Attempts: make(map[string]int)}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, errors.New("invalid state object")
	}
	if attempts, exists := shape["attempts"]; !exists || strings.TrimSpace(string(attempts)) == "null" {
		return nil, errors.New("state is missing its attempts object")
	}
	if store.state.Attempts == nil {
		store.state.Attempts = make(map[string]int)
	}
	if strings.TrimSpace(string(data)) == "null" {
		return nil, errors.New("state must be a JSON object")
	}
	for key, n := range store.state.Attempts {
		if key == "" || n < 0 {
			return nil, errors.New("invalid retry state")
		}
	}
	for key, op := range store.state.Operations {
		if key == "" || (op.Kind != "sonarr" && op.Kind != "radarr") || op.SubjectID < 1 || op.Phase == "" {
			return nil, errors.New("invalid operation journal")
		}
		if err := validateImportJournal(key, op); err != nil {
			return nil, err
		}
	}
	for key, job := range store.state.Webhooks {
		if job.Failures < 0 {
			return nil, errors.New("invalid webhook retry state")
		}
		expected, _, err := storedWebhook(job.Kind, job.Payload)
		if err != nil || key != expected {
			return nil, errors.New("invalid durable webhook")
		}
		// Legacy five-failure jobs may safely re-evaluate reads after upgrade.
		// Pending operations continue to fence every mutation.
		job.Failures = min(job.Failures, maxWebhookBackoffFailures)
		store.state.Webhooks[key] = job
	}
	// Migrate legacy episode combinations to independent counters so changing
	// release grouping cannot reset a retry cap.
	for key, n := range store.state.Attempts {
		if strings.HasPrefix(key, "sonarr:episodes:") && strings.Contains(key, ",") {
			for _, id := range strings.Split(strings.TrimPrefix(key, "sonarr:episodes:"), ",") {
				parsed, err := strconv.Atoi(id)
				if err != nil || parsed < 1 {
					return nil, errors.New("invalid legacy episode retry key")
				}
				individual := "sonarr:episodes:" + id
				if store.state.Attempts[individual] < n {
					store.state.Attempts[individual] = n
				}
			}
			delete(store.state.Attempts, key)
		}
	}
	return store, nil
}

func (s *StateStore) Attempts(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Attempts[key]
}

func (s *StateStore) Increment(key string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Attempts[key]
	if n < math.MaxInt {
		n++
	}
	err := s.updateLocked(func(next *State) { next.Attempts[key] = n })
	return s.state.Attempts[key], err
}

func (s *StateStore) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Attempts[key]; !ok {
		return nil
	}
	return s.updateLocked(func(next *State) { delete(next.Attempts, key) })
}

func (s *StateStore) updateLocked(change func(*State)) error {
	if s.writeErr != nil {
		return fmt.Errorf("state store disabled after persistence failure: %w", s.writeErr)
	}
	previous := s.state
	s.state = State{Attempts: maps.Clone(previous.Attempts), Operations: maps.Clone(previous.Operations), Completed: maps.Clone(previous.Completed), Instances: maps.Clone(previous.Instances), Webhooks: maps.Clone(previous.Webhooks)}
	if s.state.Attempts == nil {
		s.state.Attempts = map[string]int{}
	}
	if s.state.Operations == nil {
		s.state.Operations = map[string]Operation{}
	}
	if s.state.Completed == nil {
		s.state.Completed = map[string]bool{}
	}
	if s.state.Instances == nil {
		s.state.Instances = map[string]string{}
	}
	if s.state.Webhooks == nil {
		s.state.Webhooks = map[string]StoredWebhook{}
	}
	change(&s.state)
	if err := s.saveLocked(); err != nil {
		s.state = previous
		s.writeErr = err
		return err
	}
	return nil
}

func (s *StateStore) Begin(key string, op Operation, retryKeys []string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Completed[key] {
		return 0, false, nil
	}
	for _, pending := range s.state.Operations {
		if pending.Kind == op.Kind && (pending.SubjectID == op.SubjectID || (op.DownloadID != "" && strings.EqualFold(pending.DownloadID, op.DownloadID))) {
			return 0, false, requireReconciliation(errors.New("unfinished operation requires manual reconciliation in STATE_PATH"))
		}
	}
	attempt := 0
	for _, retryKey := range retryKeys {
		if n := s.state.Attempts[retryKey]; n > attempt {
			attempt = n
		}
	}
	if attempt < math.MaxInt {
		attempt++
	}
	err := s.updateLocked(func(next *State) {
		for _, retryKey := range retryKeys {
			next.Attempts[retryKey] = attempt
		}
		op.EpisodeIDs = append([]int(nil), op.EpisodeIDs...)
		next.Operations[key] = op
	})
	return attempt, err == nil, err
}

func (s *StateStore) Phase(key, phase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.state.Operations[key]
	if !ok {
		return errors.New("missing operation journal entry")
	}
	return s.updateLocked(func(next *State) { op.Phase = phase; next.Operations[key] = op })
}

func (s *StateStore) Complete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(func(next *State) { delete(next.Operations, key); next.Completed[key] = true })
}

func (s *StateStore) IsCompleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Completed[key]
}

func (s *StateStore) ImportSubmitted(key string, commandID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.state.Operations[key]
	if !ok || op.Import == nil || commandID < 1 {
		return errors.New("missing import journal or command ID")
	}
	return s.updateLocked(func(next *State) {
		pending := *op.Import
		pending.CommandID = commandID
		op.Import, op.Phase = &pending, "manual-import-submitted"
		next.Operations[key] = op
	})
}

// Record who will search before history failure can itself enqueue an Arr search.
func (s *StateStore) OriginRequested(key string, automaticSearch bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.state.Operations[key]
	if !ok {
		return errors.New("missing operation journal entry")
	}
	return s.updateLocked(func(next *State) {
		op.Phase = "origin-requested"
		op.AutomaticSearch = automaticSearch
		next.Operations[key] = op
	})
}

func (s *StateStore) SearchRequested(key string, ids []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.state.Operations[key]
	if !ok {
		return errors.New("missing operation journal entry")
	}
	return s.updateLocked(func(next *State) {
		op.Phase = "search-requested"
		op.SearchEpisodeIDs = append([]int(nil), ids...)
		next.Operations[key] = op
	})
}

func (s *StateStore) Pending() map[string]Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.state.Operations)
}

func (s *StateStore) Close() error {
	if s.lockFile != nil {
		return s.lockFile.Close()
	}
	return nil
}

func (s *StateStore) saveLocked() error {
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create state directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// writeFileAtomic writes a complete file next to the destination and then
// renames it into place. Readers see either the old JSON or the new JSON,
// never a partially written document.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceStateFile(tempPath, path)
}
