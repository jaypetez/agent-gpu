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
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: not found")

// APIKey is a stored API credential. The secret itself is NEVER stored: only a
// salted hash and its salt are persisted, so a leaked store cannot be used to
// recover usable tokens. The auth epic (#2) owns the lifecycle that populates
// these fields; permissions and quotas (#3, #6) attach to the key by ID.
type APIKey struct {
	// ID is the opaque, public key identifier (the "keyid" in the token). It is
	// stored in plaintext and used for O(1) lookup; it is NOT the secret.
	ID string
	// Name is a human-readable label for the key.
	Name string
	// Prefix is the token namespace prefix (e.g. "agpu").
	Prefix string
	// SecretHash is SHA-256(Salt || secret). The plaintext secret is never stored.
	SecretHash []byte
	// Salt is the per-key random salt mixed into SecretHash.
	Salt []byte
	// CreatedAt is when the key was first created.
	CreatedAt time.Time
	// LastUsedAt is the timestamp of the most recent successful authentication.
	LastUsedAt time.Time
	// UsageCount is the number of successful authentications.
	UsageCount uint64
	// RevokedAt, when non-nil, marks the key as revoked; revoked keys never
	// authenticate.
	RevokedAt *time.Time
}

// Revoked reports whether the key has been revoked.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

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

// cloneAPIKey returns a deep copy so that callers and the store never share
// mutable backing arrays (the secret-hash/salt slices, the revoked pointer).
func cloneAPIKey(k APIKey) APIKey {
	out := k
	if k.SecretHash != nil {
		out.SecretHash = append([]byte(nil), k.SecretHash...)
	}
	if k.Salt != nil {
		out.Salt = append([]byte(nil), k.Salt...)
	}
	if k.RevokedAt != nil {
		t := *k.RevokedAt
		out.RevokedAt = &t
	}
	return out
}

// PutAPIKey implements Store.
func (m *Memory) PutAPIKey(_ context.Context, key APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key.ID] = cloneAPIKey(key)
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
	return cloneAPIKey(k), nil
}

// ListAPIKeys implements Store.
func (m *Memory) ListAPIKeys(_ context.Context) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]APIKey, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, cloneAPIKey(k))
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
