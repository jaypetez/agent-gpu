package audit

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendAndList proves the store records entries and List returns them
// newest-first with their fields intact.
func TestAppendAndList(t *testing.T) {
	s := NewMemoryStore(0)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	for i, op := range []string{"key.create", "key.revoke", "worker.drain"} {
		if err := s.Append(Entry{
			Time:    base.Add(time.Duration(i) * time.Minute),
			Actor:   "admin1",
			Op:      op,
			Target:  "t" + op,
			Outcome: OutcomeSuccess,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got := s.List(Filter{}, 0)
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	// Newest first.
	if got[0].Op != "worker.drain" || got[2].Op != "key.create" {
		t.Errorf("not newest-first: %s ... %s", got[0].Op, got[2].Op)
	}
	if got[0].Actor != "admin1" {
		t.Errorf("actor = %q, want admin1", got[0].Actor)
	}
}

// TestFilter proves the Filter ANDs its fields and applies the half-open time
// window.
func TestFilter(t *testing.T) {
	s := NewMemoryStore(0)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Time: base, Actor: "a", Op: "key.create", Target: "k1", Outcome: OutcomeSuccess},
		{Time: base.Add(time.Minute), Actor: "b", Op: "key.revoke", Target: "k1", Outcome: OutcomeSuccess},
		{Time: base.Add(2 * time.Minute), Actor: "a", Op: "key.revoke", Target: "k2", Outcome: OutcomeFailure},
	}
	for _, e := range entries {
		_ = s.Append(e)
	}

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"all", Filter{}, 3},
		{"by actor", Filter{Actor: "a"}, 2},
		{"by op", Filter{Op: "key.revoke"}, 2},
		{"by target", Filter{Target: "k1"}, 2},
		{"actor and op", Filter{Actor: "a", Op: "key.revoke"}, 1},
		{"since inclusive", Filter{Since: base.Add(time.Minute)}, 2},
		{"until exclusive", Filter{Until: base.Add(2 * time.Minute)}, 2},
		{"window", Filter{Since: base.Add(time.Minute), Until: base.Add(2 * time.Minute)}, 1},
		{"no match", Filter{Actor: "ghost"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := len(s.List(c.filter, 0)); got != c.want {
				t.Errorf("List(%+v) len = %d, want %d", c.filter, got, c.want)
			}
		})
	}
}

// TestListLimit proves the limit caps the returned page (newest-first).
func TestListLimit(t *testing.T) {
	s := NewMemoryStore(0)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = s.Append(Entry{Time: base.Add(time.Duration(i) * time.Minute), Op: "x", Outcome: OutcomeSuccess})
	}
	got := s.List(Filter{}, 2)
	if len(got) != 2 {
		t.Fatalf("limit=2 returned %d", len(got))
	}
	// Newest two are minutes 4 and 3.
	if !got[0].Time.Equal(base.Add(4*time.Minute)) || !got[1].Time.Equal(base.Add(3*time.Minute)) {
		t.Errorf("limit did not return the newest entries: %v", got)
	}
}

// TestCapEvictsOldest proves the rolling-window cap drops the oldest entries.
func TestCapEvictsOldest(t *testing.T) {
	s := NewMemoryStore(3)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		_ = s.Append(Entry{Time: base.Add(time.Duration(i) * time.Minute), Op: "x", Target: string(rune('a' + i)), Outcome: OutcomeSuccess})
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (capped)", s.Len())
	}
	got := s.List(Filter{}, 0)
	// The three newest survive (minutes 3,4,5 → targets d,e,f); oldest dropped.
	if got[0].Target != "f" || got[2].Target != "d" {
		t.Errorf("cap kept wrong window: %v", targets(got))
	}
}

// TestAppendStampsTime proves a zero-time entry is stamped on append.
func TestAppendStampsTime(t *testing.T) {
	s := NewMemoryStore(0)
	_ = s.Append(Entry{Op: "x", Outcome: OutcomeSuccess})
	got := s.List(Filter{}, 0)
	if len(got) != 1 || got[0].Time.IsZero() {
		t.Fatalf("append did not stamp time: %+v", got)
	}
}

// TestListReturnsCopies proves a caller cannot mutate stored state through a
// returned entry's before/after map.
func TestListReturnsCopies(t *testing.T) {
	s := NewMemoryStore(0)
	_ = s.Append(Entry{Op: "x", Outcome: OutcomeSuccess, After: RedactedValues{"roles": []string{"user"}}})

	got := s.List(Filter{}, 0)
	got[0].After["roles"] = "tampered"

	again := s.List(Filter{}, 0)
	if again[0].After["roles"] == "tampered" {
		t.Fatal("List did not return an isolated copy; stored entry was mutated")
	}
}

// TestRedactionNoSecretInEntry proves the before/after projection (and JSON
// serialization) never carries secret material. This is the secret-hygiene
// assertion for the audit log (AC3): an entry built from the safe key fields and
// marshaled must not contain a hash/salt/token.
func TestRedactionNoSecretInEntry(t *testing.T) {
	s := NewMemoryStore(0)
	_ = s.Append(Entry{
		Actor:   "admin1",
		Op:      "key.create",
		Target:  "k1",
		Outcome: OutcomeSuccess,
		After: RedactedValues{
			"id":           "k1",
			"name":         "app",
			"roles":        []string{"user"},
			"admin_scopes": []string{"keys:read"},
		},
	})

	data, err := json.Marshal(s.List(Filter{}, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := strings.ToLower(string(data))
	for _, banned := range []string{"secrethash", "secret_hash", "\"salt\"", "token"} {
		if strings.Contains(body, banned) {
			t.Fatalf("audit entry leaks secret material (%q): %s", banned, body)
		}
	}
}

// TestLogValueElidesValues proves Entry.LogValue emits only the safe scalar
// fields — never the before/after VALUES — so an accidental slog of a whole
// entry cannot widen the surface.
func TestLogValueElidesValues(t *testing.T) {
	e := Entry{
		Actor:   "admin1",
		Op:      "key.create",
		Target:  "k1",
		Outcome: OutcomeSuccess,
		After:   RedactedValues{"name": "supersecretname"},
	}
	v := e.LogValue()
	// The grouped value must contain actor/op/target/outcome but not the After map.
	rendered := v.String()
	if !strings.Contains(rendered, "admin1") || !strings.Contains(rendered, "key.create") {
		t.Errorf("LogValue missing safe fields: %s", rendered)
	}
	if strings.Contains(rendered, "supersecretname") {
		t.Errorf("LogValue leaked a before/after value: %s", rendered)
	}
}

// TestCheckpointRoundTrip proves entries persist and reload (AC4): a fresh store
// loading the checkpoint sees exactly the entries written, in order.
func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")

	s1 := NewMemoryStore(0)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	for i, op := range []string{"key.create", "key.revoke"} {
		_ = s1.Append(Entry{
			Time:    base.Add(time.Duration(i) * time.Minute),
			Actor:   "admin1",
			Op:      op,
			Target:  "k1",
			Outcome: OutcomeSuccess,
			After:   RedactedValues{"id": "k1"},
		})
	}
	if err := s1.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	s2 := NewMemoryStore(0)
	if err := s2.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := s2.List(Filter{}, 0)
	if len(got) != 2 {
		t.Fatalf("reloaded %d entries, want 2", len(got))
	}
	if got[0].Op != "key.revoke" || got[1].Op != "key.create" {
		t.Errorf("reloaded order wrong: %v", []string{got[0].Op, got[1].Op})
	}
	if got[1].After["id"] != "k1" {
		t.Errorf("reloaded before/after lost: %+v", got[1].After)
	}
}

// TestLoadCheckpointMissingFile proves a missing checkpoint is not an error (a
// fresh store), mirroring the quota/session stores.
func TestLoadCheckpointMissingFile(t *testing.T) {
	s := NewMemoryStore(0)
	if err := s.LoadCheckpoint(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

// TestLoadCheckpointReappliesCap proves a checkpoint larger than the cap is
// trimmed to the newest maxEntries on load.
func TestLoadCheckpointReappliesCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")

	big := NewMemoryStore(0)
	base := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = big.Append(Entry{Time: base.Add(time.Duration(i) * time.Minute), Op: "x", Target: string(rune('a' + i)), Outcome: OutcomeSuccess})
	}
	if err := big.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	small := NewMemoryStore(2)
	if err := small.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if small.Len() != 2 {
		t.Fatalf("Len = %d, want 2 after cap re-apply", small.Len())
	}
	got := small.List(Filter{}, 0)
	if got[0].Target != "e" || got[1].Target != "d" {
		t.Errorf("cap re-apply kept wrong window: %v", targets(got))
	}
}

// TestCheckpointEmptyPath proves an empty path is rejected.
func TestCheckpointEmptyPath(t *testing.T) {
	if err := NewMemoryStore(0).Checkpoint(""); err == nil {
		t.Fatal("empty path should error")
	}
}

func targets(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Target
	}
	return out
}
