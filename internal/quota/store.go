package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CounterStore holds per-key fixed-window counters. Implementations MUST be
// safe for concurrent use and MUST serialize the check-and-increment performed
// by Reserve so that, under concurrency, exactly the permitted number of
// requests succeed (no lost updates).
//
// The in-memory implementation (MemoryCounterStore) ships today; the interface
// is shaped so a Redis-backed implementation (atomic INCR + per-key window
// keys) can slot in later without touching the Engine. Redis is out of scope
// for #5.
type CounterStore interface {
	// Reserve rolls keyID's windows for now, then atomically evaluates the
	// supplied check against the rolled counters. If check returns nil, Reserve
	// applies the request reservation (MinuteRequests++) and persists the
	// rolled+incremented counters, returning nil. If check returns an error,
	// Reserve still persists the rolled (reset) counters — so an expired window
	// is observed as reset — but does NOT increment, and returns check's error.
	//
	// check receives a copy of the rolled counters and must not retain it.
	// The whole operation is serialized per key.
	Reserve(ctx context.Context, keyID string, now time.Time, check func(Counters) error) error

	// AddTokens rolls keyID's windows for now, then adds n tokens to the
	// minute/day/month token counters, persisting the result. It is serialized
	// per key.
	AddTokens(ctx context.Context, keyID string, now time.Time, n uint64) error

	// Get rolls keyID's windows for now and returns a copy of the resulting
	// counters. A key with no recorded usage yields zero counters whose window
	// starts are aligned to now.
	Get(ctx context.Context, keyID string, now time.Time) (Counters, error)
}

// MemoryCounterStore is an in-memory, concurrency-safe CounterStore. A single
// mutex serializes all operations, which guarantees the per-key
// check-and-increment is atomic (the simplest correct design for the modest
// key counts a control plane manages; a Redis backend would shard per key).
//
// Counters survive process restarts via Checkpoint/LoadCheckpoint to a JSON
// file: the engine checkpoints periodically and on graceful shutdown, and loads
// the checkpoint on startup (rolling any expired windows lazily on first
// access). Per-request writes never touch disk.
type MemoryCounterStore struct {
	mu       sync.Mutex
	counters map[string]Counters
}

// NewMemoryCounterStore returns an empty in-memory CounterStore.
func NewMemoryCounterStore() *MemoryCounterStore {
	return &MemoryCounterStore{counters: make(map[string]Counters)}
}

// Reserve implements CounterStore.
func (m *MemoryCounterStore) Reserve(_ context.Context, keyID string, now time.Time, check func(Counters) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.counters[keyID]
	c.roll(now)
	if err := check(c); err != nil {
		// Persist the rolled (reset) state so an expired window is not
		// re-observed as full on the next call, but do not reserve.
		m.counters[keyID] = c
		return err
	}
	c.MinuteRequests++
	m.counters[keyID] = c
	return nil
}

// AddTokens implements CounterStore.
func (m *MemoryCounterStore) AddTokens(_ context.Context, keyID string, now time.Time, n uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.counters[keyID]
	c.roll(now)
	c.MinuteTokens += n
	c.DayTokens += n
	c.MonthTokens += n
	m.counters[keyID] = c
	return nil
}

// Get implements CounterStore.
func (m *MemoryCounterStore) Get(_ context.Context, keyID string, now time.Time) (Counters, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.counters[keyID]
	c.roll(now)
	m.counters[keyID] = c
	return c, nil
}

// checkpointMode is the permission bits for the checkpoint file (owner only),
// matching the keys file: it records usage, not secrets, but defense in depth
// is cheap.
const checkpointMode os.FileMode = 0o600

// checkpointDirMode is the permission bits for the checkpoint parent directory.
const checkpointDirMode os.FileMode = 0o700

// Checkpoint atomically writes the current counters to path (write temp +
// rename), creating the parent directory if needed. It rolls each key's windows
// to now first so the persisted snapshot reflects current (not stale) windows.
func (m *MemoryCounterStore) Checkpoint(path string, now time.Time) error {
	if path == "" {
		return fmt.Errorf("quota: empty checkpoint path")
	}

	m.mu.Lock()
	snapshot := make(map[string]Counters, len(m.counters))
	for id, c := range m.counters {
		c.roll(now)
		m.counters[id] = c
		snapshot[id] = c
	}
	m.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("quota: marshal checkpoint: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, checkpointDirMode); err != nil {
		return fmt.Errorf("quota: create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".quota-*.tmp")
	if err != nil {
		return fmt.Errorf("quota: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(checkpointMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("quota: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("quota: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("quota: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("quota: rename checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint replaces the store's counters with those persisted at path. A
// missing file is not an error (a fresh store). Windows are rolled lazily on
// first access (Reserve/AddTokens/Get), so usage recorded in a now-expired
// window is correctly discarded on first use after restart.
func (m *MemoryCounterStore) LoadCheckpoint(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("quota: read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var loaded map[string]Counters
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("quota: parse checkpoint %s: %w", path, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if loaded == nil {
		loaded = make(map[string]Counters)
	}
	m.counters = loaded
	return nil
}

// Compile-time assertion that MemoryCounterStore satisfies CounterStore.
var _ CounterStore = (*MemoryCounterStore)(nil)
