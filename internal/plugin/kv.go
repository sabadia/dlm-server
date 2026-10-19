package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	maxKVBytes = 256 << 10
	maxKVKey   = 128
)

// kvStore is the small persistent store behind dlm.storage: one JSON file.
type kvStore struct {
	path string
	mu   sync.Mutex
}

func newKV(path string) *kvStore { return &kvStore{path: path} }

func (s *kvStore) load() (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dlm.storage: %w", err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("dlm.storage: the stored data is damaged: %w", err)
	}
	return m, nil
}

func (s *kvStore) save(m map[string]json.RawMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("dlm.storage: %w", err)
	}
	if len(data) > maxKVBytes {
		return fmt.Errorf("dlm.storage: the plugin's storage would exceed %d bytes", maxKVBytes)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("dlm.storage: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".kv-*")
	if err != nil {
		return fmt.Errorf("dlm.storage: %w", err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("dlm.storage: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("dlm.storage: %w", err)
	}
	return nil
}

func checkKey(k string) error {
	if k == "" || len(k) > maxKVKey {
		return fmt.Errorf("dlm.storage: a key must have 1-%d characters", maxKVKey)
	}
	return nil
}

func (s *kvStore) get(k string) (any, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, false, err
	}
	raw, ok := m[k]
	if !ok {
		return nil, false, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, fmt.Errorf("dlm.storage: %w", err)
	}
	return v, true, nil
}

func (s *kvStore) set(k string, v any) error {
	if err := checkKey(k); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("dlm.storage: the value cannot be stored as JSON: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[k] = raw
	return s.save(m)
}

func (s *kvStore) delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[k]; !ok {
		return nil
	}
	delete(m, k)
	return s.save(m)
}

func (s *kvStore) keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
