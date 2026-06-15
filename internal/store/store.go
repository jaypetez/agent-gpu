// Package store defines the persistence seam for agent-gpu's control-plane
// state: API keys, quotas, and permissions.
//
// This issue (#1) only establishes the interface and a minimal in-memory
// implementation. It deliberately does NOT implement real auth, quota, or
// permission *logic* — those are filled in by later epics. The point here is
// to nail down the seam so the server depends on an interface, not a concrete
// backend (Redis/Postgres/embedded), from day one.
package store

import (
	"context"
	"errors"
	"sync"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: not found")

// APIKey is a stored API credential. Fields beyond the identifier are added by
// the auth epic; for now this is just enough to exercise the seam.
type APIKey struct {
	// ID is the opaque key identifier (not the secret itself).
	ID string
	// Name is a human-readable label for the key.
	Name string
}

// Store is the persistence interface for control-plane state. Implementations
// must be safe for concurrent use. Real backends (Redis/Postgres) and the
// associated auth/quota/permission semantics are introduced by later epics.
type Store interface {
	// PutAPIKey stores (or overwrites) an API key record.
	PutAPIKey(ctx context.Context, key APIKey) error
	// GetAPIKey returns the key with the given id, or ErrNotFound.
	GetAPIKey(ctx context.Context, id string) (APIKey, error)
	// ListAPIKeys returns all stored keys.
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
	// DeleteAPIKey removes a key. Deleting a missing key is not an error.
	DeleteAPIKey(ctx context.Context, id string) error

	// Close releases any resources held by the store.
	Close() error
}

// Memory is an in-memory, concurrency-safe Store implementation. It is the
// default backend for standalone/dev use and the substrate for tests.
type Memory struct {
	mu   sync.RWMutex
	keys map[string]APIKey
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{keys: make(map[string]APIKey)}
}

// PutAPIKey implements Store.
func (m *Memory) PutAPIKey(_ context.Context, key APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key.ID] = key
	return nil
}

// GetAPIKey implements Store.
func (m *Memory) GetAPIKey(_ context.Context, id string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok {
		return APIKey{}, ErrNotFound
	}
	return k, nil
}

// ListAPIKeys implements Store.
func (m *Memory) ListAPIKeys(_ context.Context) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]APIKey, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k)
	}
	return out, nil
}

// DeleteAPIKey implements Store.
func (m *Memory) DeleteAPIKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, id)
	return nil
}

// Close implements Store. The in-memory store holds no external resources.
func (m *Memory) Close() error { return nil }

// Compile-time assertion that Memory satisfies Store.
var _ Store = (*Memory)(nil)
