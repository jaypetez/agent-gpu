package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File is a JSON-file-backed, concurrency-safe Store. It is the minimal
// persistence implementation that lets the CLI share key state across
// processes; richer backends (Redis/Postgres) are deferred to later epics. The
// whole keyset is held in memory and rewritten atomically on each mutation,
// which is fine for the modest key counts a control plane manages.
//
// The keys file is created with 0600 permissions and its parent directory with
// 0700, since it contains salted secret hashes.
type File struct {
	mu   sync.RWMutex
	path string
	keys map[string]APIKey
}

// fileMode is the permission bits for the keys file (owner read/write only).
const fileMode os.FileMode = 0o600

// dirMode is the permission bits for the parent directory (owner only).
const dirMode os.FileMode = 0o700

// NewFile opens (or creates) a JSON-backed Store at path. The parent directory
// is created if missing. A missing file is treated as an empty store.
func NewFile(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	f := &File{path: path, keys: make(map[string]APIKey)}
	if err := f.load(); err != nil {
		return nil, err
	}
	return f, nil
}

// load reads the keys file into memory. A non-existent file is not an error.
func (f *File) load() error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: read %s: %w", f.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var records []APIKey
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("store: parse %s: %w", f.path, err)
	}
	for _, k := range records {
		f.keys[k.ID] = k
	}
	return nil
}

// flush writes the in-memory keyset to disk atomically (write temp + rename),
// preserving the restrictive file mode. Callers must hold the write lock.
func (f *File) flush() error {
	records := make([]APIKey, 0, len(f.keys))
	for _, k := range f.keys {
		records = append(records, k)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close temp: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("store: rename: %w", err)
	}
	return nil
}

// PutAPIKey implements Store.
func (f *File) PutAPIKey(_ context.Context, key APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key.ID] = cloneAPIKey(key)
	return f.flush()
}

// GetAPIKey implements Store.
func (f *File) GetAPIKey(_ context.Context, id string) (APIKey, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	k, ok := f.keys[id]
	if !ok {
		return APIKey{}, ErrNotFound
	}
	return cloneAPIKey(k), nil
}

// ListAPIKeys implements Store.
func (f *File) ListAPIKeys(_ context.Context) ([]APIKey, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]APIKey, 0, len(f.keys))
	for _, k := range f.keys {
		out = append(out, cloneAPIKey(k))
	}
	return out, nil
}

// DeleteAPIKey implements Store. Deleting a missing key is not an error.
func (f *File) DeleteAPIKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys[id]; !ok {
		return nil
	}
	delete(f.keys, id)
	return f.flush()
}

// Close implements Store. State is flushed on every mutation, so there is
// nothing to release.
func (f *File) Close() error { return nil }

// Compile-time assertion that File satisfies Store.
var _ Store = (*File)(nil)
