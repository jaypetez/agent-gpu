// Package usage captures a lightweight, bounded historical series of per-key
// token consumption for the admin usage/quota reporting API (#97). It is the
// time-series complement to the quota engine's point-in-time Snapshot: the quota
// package (internal/quota) owns the live counters and their fixed-window reset
// math, while this package keeps a short rolling history — one DAILY sample per
// key — so a dashboard can render a sparkline and a best-effort exhaustion
// forecast without standing up a metrics/time-series database.
//
// # Granularity and retention
//
// The series is sampled at DAILY granularity, aligned to the same UTC-midnight
// day boundary the quota engine's daily token budget uses (so a sample's day
// matches the "tokens today" reset window). Per key, the store keeps a bounded
// ring of at most MaxDays daily samples; recording a sample for a new day drops
// the oldest so the per-key history never exceeds MaxDays. The whole store is
// therefore bounded by construction (number of keys × MaxDays), needs no
// eviction policy beyond the per-key day cap, and lives entirely in memory with a
// file checkpoint (NO database) — mirroring the quota counter store's persistence
// discipline.
//
// # What a sample records
//
// Each DailySample records the cumulative TOKENS the key consumed THAT day
// (TokensToday from the quota snapshot) plus the requests reserved in the current
// minute window at sample time. The daily token figure is what a forecast and a
// sparkline care about; it is captured verbatim from the snapshot rather than
// re-derived, so the reported history is exactly what enforcement saw.
//
// # Concurrency and persistence
//
// The Store is safe for concurrent use (a single mutex serializes Record, the
// per-key reads, and the checkpoint snapshot). Checkpoint/LoadCheckpoint persist
// the whole series to a JSON file with the same atomic temp-write + rename
// discipline and owner-only permissions as the quota checkpoint
// (internal/quota/store.go); a missing file is not an error (a fresh store).
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
)

// MaxDays is the bound on how many daily samples the store retains per key. It is
// >= 7 so the series always covers at least a week (the acceptance criterion),
// while staying small so the in-memory footprint is number-of-keys × MaxDays.
const MaxDays = 7

// DailySample is one day's usage point for a key: the UTC day it covers and the
// cumulative tokens the key consumed that day, plus the requests reserved in the
// minute window observed at sample time. Day is the UTC-midnight start of the day
// (matching the quota daily-budget window), serialized as RFC3339 so the
// checkpoint is human-readable and unambiguous across time zones.
type DailySample struct {
	// Day is the UTC-midnight start of the day this sample covers.
	Day time.Time `json:"day"`
	// Tokens is the cumulative tokens the key consumed during Day (the quota
	// snapshot's TokensToday at sample time).
	Tokens uint64 `json:"tokens"`
	// Requests is the requests reserved in the current minute window at sample
	// time (the snapshot's RequestsThisMinute). It is a coarse activity signal for
	// the sparkline, not a daily total (the quota engine does not retain a daily
	// request count), so callers should treat Tokens as the primary series.
	Requests uint64 `json:"requests"`
}

// Store holds a bounded rolling daily usage series per key. The zero value is not
// usable; construct it with New. It is safe for concurrent use.
type Store struct {
	mu sync.Mutex
	// series maps a key id to its day-ordered ring of samples (oldest first), each
	// capped at MaxDays entries. A key appears only once it has been recorded.
	series map[string][]DailySample
}

// New returns an empty usage series store.
func New() *Store {
	return &Store{series: make(map[string][]DailySample)}
}

// dayStart returns the UTC-midnight start of the day containing t, matching the
// quota engine's daily-budget window boundary so a sample's day lines up with the
// "tokens today" reset.
func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Record folds one round of quota snapshots into the series at now's UTC day. For
// each snapshot it updates that key's sample for today (overwriting today's
// figure with the latest cumulative TokensToday/RequestsThisMinute, since the
// snapshot is cumulative for the day), appending a new day when the day has rolled
// and dropping the oldest sample so the per-key history stays at most MaxDays.
//
// Recording is idempotent within a day: repeated calls on the same UTC day update
// today's single sample in place rather than appending duplicates, so the sample
// cadence (how often the snapshotter runs) does not inflate the series — only the
// day boundary does. A snapshot whose KeyID is empty is skipped.
func (s *Store) Record(snaps []quota.Snapshot, now time.Time) {
	today := dayStart(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, snap := range snaps {
		if snap.KeyID == "" {
			continue
		}
		sample := DailySample{
			Day:      today,
			Tokens:   snap.TokensToday,
			Requests: snap.RequestsThisMinute,
		}
		hist := s.series[snap.KeyID]
		switch {
		case len(hist) > 0 && hist[len(hist)-1].Day.Equal(today):
			// Same day: refresh today's cumulative figures in place.
			hist[len(hist)-1] = sample
		default:
			// A new (later) day, or the first sample for this key: append, then trim
			// the oldest so the ring stays bounded at MaxDays.
			hist = append(hist, sample)
			if len(hist) > MaxDays {
				hist = hist[len(hist)-MaxDays:]
			}
		}
		s.series[snap.KeyID] = hist
	}
}

// SeriesForKey returns a copy of keyID's daily samples, oldest first, or an empty
// (non-nil) slice when the key has no recorded history. The copy means a caller
// can read/sort/render the result without holding the lock or risking a race with
// a concurrent Record.
func (s *Store) SeriesForKey(keyID string) []DailySample {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.series[keyID]
	out := make([]DailySample, len(hist))
	copy(out, hist)
	return out
}

// checkpointMode / checkpointDirMode are the owner-only permission bits for the
// checkpoint file and its parent directory, matching the quota/audit/config
// checkpoints. The series records usage, not secrets, but defense in depth is
// cheap and keeps the state directory uniformly locked down.
const (
	checkpointMode    os.FileMode = 0o600
	checkpointDirMode os.FileMode = 0o700
)

// Checkpoint atomically writes the current series to path (write temp + rename),
// creating the parent directory if needed. An empty path is a no-op success so a
// store wired without persistence (or a test) never touches disk. The on-disk
// format and atomic-write discipline mirror the quota counter checkpoint
// (internal/quota/store.go).
func (s *Store) Checkpoint(path string) error {
	if path == "" {
		return nil
	}

	s.mu.Lock()
	snapshot := make(map[string][]DailySample, len(s.series))
	for id, hist := range s.series {
		cp := make([]DailySample, len(hist))
		copy(cp, hist)
		snapshot[id] = cp
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("usage: marshal checkpoint: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, checkpointDirMode); err != nil {
		return fmt.Errorf("usage: create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".usage-*.tmp")
	if err != nil {
		return fmt.Errorf("usage: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(checkpointMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("usage: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("usage: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("usage: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("usage: rename checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint replaces the store's series with the one persisted at path. A
// missing or empty file is not an error (a fresh store). Each key's loaded history
// is defensively re-sorted by day and trimmed to the most recent MaxDays, so a
// hand-edited or older-format file (e.g. one written when MaxDays was larger)
// cannot make the in-memory ring exceed its bound or fall out of day order.
func (s *Store) LoadCheckpoint(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("usage: read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var loaded map[string][]DailySample
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("usage: parse checkpoint %s: %w", path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.series = make(map[string][]DailySample, len(loaded))
	for id, hist := range loaded {
		// Defensive: normalize each day to its UTC-midnight start, sort oldest-first,
		// and keep only the most recent MaxDays so the loaded ring obeys the same
		// invariants Record maintains.
		for i := range hist {
			hist[i].Day = dayStart(hist[i].Day)
		}
		sort.Slice(hist, func(i, j int) bool { return hist[i].Day.Before(hist[j].Day) })
		if len(hist) > MaxDays {
			hist = hist[len(hist)-MaxDays:]
		}
		s.series[id] = hist
	}
	return nil
}
