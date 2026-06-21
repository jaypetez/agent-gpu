package usage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/quota"
)

// day returns a fixed UTC instant n days after a stable epoch, used to drive the
// daily-rollover logic deterministically (no wall clock).
func day(n int) time.Time {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return base.AddDate(0, 0, n)
}

// snap is a tiny constructor for a quota snapshot carrying just the fields the
// series records (KeyID + TokensToday + RequestsThisMinute).
func snap(keyID string, tokensToday, requests uint64) quota.Snapshot {
	return quota.Snapshot{
		KeyID:              keyID,
		TokensToday:        tokensToday,
		RequestsThisMinute: requests,
	}
}

// TestRecordDailyRollAndBound is the table-driven proof that Record keeps one
// sample per UTC day per key, refreshes today's sample in place (so the sample
// cadence does not inflate the series), advances on a new day, and never exceeds
// MaxDays — dropping the oldest day when an 8th is recorded.
func TestRecordDailyRollAndBound(t *testing.T) {
	cases := []struct {
		name string
		// records is the sequence of (day, tokensToday) folded in for key "k".
		records []struct {
			d      int
			tokens uint64
		}
		wantDays   []int    // expected sample days (offsets), oldest first
		wantTokens []uint64 // expected tokens aligned with wantDays
	}{
		{
			name: "same day refreshes in place (no duplicate samples)",
			records: []struct {
				d      int
				tokens uint64
			}{{0, 10}, {0, 25}, {0, 40}},
			wantDays:   []int{0},
			wantTokens: []uint64{40},
		},
		{
			name: "distinct days append in order",
			records: []struct {
				d      int
				tokens uint64
			}{{0, 10}, {1, 20}, {2, 35}},
			wantDays:   []int{0, 1, 2},
			wantTokens: []uint64{10, 20, 35},
		},
		{
			name: "an eighth day drops the oldest (bounded at MaxDays=7)",
			records: []struct {
				d      int
				tokens uint64
			}{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}, {5, 6}, {6, 7}, {7, 8}},
			// Day 0 dropped; days 1..7 retained (7 samples).
			wantDays:   []int{1, 2, 3, 4, 5, 6, 7},
			wantTokens: []uint64{2, 3, 4, 5, 6, 7, 8},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			for _, r := range tc.records {
				s.Record([]quota.Snapshot{snap("k", r.tokens, 1)}, day(r.d))
			}
			got := s.SeriesForKey("k")
			if len(got) != len(tc.wantDays) {
				t.Fatalf("series len = %d, want %d: %+v", len(got), len(tc.wantDays), got)
			}
			for i := range got {
				if !got[i].Day.Equal(dayStart(day(tc.wantDays[i]))) {
					t.Errorf("sample[%d].Day = %v, want day offset %d", i, got[i].Day, tc.wantDays[i])
				}
				if got[i].Tokens != tc.wantTokens[i] {
					t.Errorf("sample[%d].Tokens = %d, want %d", i, got[i].Tokens, tc.wantTokens[i])
				}
			}
		})
	}
}

// TestRecordMultipleKeys proves Record folds a batch of snapshots into the right
// per-key series independently, and that an empty-KeyID snapshot is skipped.
func TestRecordMultipleKeys(t *testing.T) {
	s := New()
	s.Record([]quota.Snapshot{
		snap("a", 100, 2),
		snap("b", 200, 5),
		snap("", 999, 9), // skipped: no key id
	}, day(0))

	if got := s.SeriesForKey("a"); len(got) != 1 || got[0].Tokens != 100 || got[0].Requests != 2 {
		t.Errorf("key a series = %+v, want one sample tokens=100 requests=2", got)
	}
	if got := s.SeriesForKey("b"); len(got) != 1 || got[0].Tokens != 200 {
		t.Errorf("key b series = %+v, want one sample tokens=200", got)
	}
	// The empty-key snapshot created no series.
	if got := s.SeriesForKey(""); len(got) != 0 {
		t.Errorf("empty key series = %+v, want none", got)
	}
}

// TestSeriesForKeyIsCopy proves SeriesForKey returns a defensive copy: mutating
// the returned slice does not corrupt the store's state.
func TestSeriesForKeyIsCopy(t *testing.T) {
	s := New()
	s.Record([]quota.Snapshot{snap("k", 50, 1)}, day(0))
	got := s.SeriesForKey("k")
	got[0].Tokens = 99999
	if again := s.SeriesForKey("k"); again[0].Tokens != 50 {
		t.Errorf("store mutated through returned slice: tokens = %d, want 50", again[0].Tokens)
	}
}

// TestCheckpointRoundTrip is the criterion-3 proof: a store's series survives a
// Checkpoint → LoadCheckpoint cycle into a fresh store, preserving each key's
// (bounded) daily samples in order.
func TestCheckpointRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	src := New()
	// Two keys, one with multiple days.
	for d := 0; d < 3; d++ {
		src.Record([]quota.Snapshot{snap("k1", uint64((d+1)*100), uint64(d))}, day(d))
	}
	src.Record([]quota.Snapshot{snap("k2", 42, 7)}, day(0))

	if err := src.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	dst := New()
	if err := dst.LoadCheckpoint(path); err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}

	k1 := dst.SeriesForKey("k1")
	if len(k1) != 3 {
		t.Fatalf("k1 reloaded len = %d, want 3: %+v", len(k1), k1)
	}
	for i, p := range k1 {
		if !p.Day.Equal(dayStart(day(i))) || p.Tokens != uint64((i+1)*100) {
			t.Errorf("k1[%d] = %+v, want day %d tokens %d", i, p, i, (i+1)*100)
		}
	}
	if k2 := dst.SeriesForKey("k2"); len(k2) != 1 || k2[0].Tokens != 42 || k2[0].Requests != 7 {
		t.Errorf("k2 reloaded = %+v, want one sample tokens=42 requests=7", k2)
	}
}

// TestCheckpointDropsEighthDayOnReload proves an over-long persisted series (e.g.
// a hand-edited file or one written when MaxDays was larger) is defensively
// trimmed to the most recent MaxDays on load, dropping the oldest — so the
// in-memory ring always obeys its bound even from a foreign file.
func TestCheckpointDropsEighthDayOnReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	// Build a store and Record 8 distinct days; Record itself bounds to 7, so write
	// the file, then assert reload keeps exactly the most recent 7 and drops day 0.
	src := New()
	for d := 0; d < 8; d++ {
		src.Record([]quota.Snapshot{snap("k", uint64(d+1), 1)}, day(d))
	}
	if err := src.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	dst := New()
	if err := dst.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := dst.SeriesForKey("k")
	if len(got) != MaxDays {
		t.Fatalf("reloaded len = %d, want %d", len(got), MaxDays)
	}
	// The oldest retained day is day 1 (day 0 dropped), the newest day 7.
	if !got[0].Day.Equal(dayStart(day(1))) {
		t.Errorf("oldest reloaded day = %v, want day offset 1 (day 0 must be dropped)", got[0].Day)
	}
	if !got[len(got)-1].Day.Equal(dayStart(day(7))) {
		t.Errorf("newest reloaded day = %v, want day offset 7", got[len(got)-1].Day)
	}
}

// TestLoadCheckpointMissingFile proves a missing or empty checkpoint is not an
// error (a fresh store), and that an empty path is a no-op for both ops.
func TestLoadCheckpointMissingFile(t *testing.T) {
	s := New()
	if err := s.LoadCheckpoint(filepath.Join(t.TempDir(), "does-not-exist.json")); err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if err := s.LoadCheckpoint(""); err != nil {
		t.Errorf("empty path load should be a no-op, got %v", err)
	}
	if err := s.Checkpoint(""); err != nil {
		t.Errorf("empty path checkpoint should be a no-op, got %v", err)
	}
}

// TestLoadCheckpointReplacesState proves LoadCheckpoint replaces (not merges) the
// store's contents, so a reload reflects exactly the persisted file.
func TestLoadCheckpointReplacesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	persisted := New()
	persisted.Record([]quota.Snapshot{snap("only", 5, 1)}, day(0))
	if err := persisted.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	live := New()
	live.Record([]quota.Snapshot{snap("stale", 999, 1)}, day(0))
	if err := live.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := live.SeriesForKey("stale"); len(got) != 0 {
		t.Errorf("stale key survived a load that should have replaced state: %+v", got)
	}
	if got := live.SeriesForKey("only"); len(got) != 1 {
		t.Errorf("persisted key missing after load: %+v", got)
	}
}
