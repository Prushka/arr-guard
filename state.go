package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type StateStore struct {
	path  string
	mu    sync.Mutex
	state State
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
	if store.state.Attempts == nil {
		store.state.Attempts = make(map[string]int)
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
	s.state.Attempts[key]++
	return s.state.Attempts[key], s.saveLocked()
}

func (s *StateStore) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Attempts[key]; !ok {
		return nil
	}
	delete(s.state.Attempts, key)
	return s.saveLocked()
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
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}
