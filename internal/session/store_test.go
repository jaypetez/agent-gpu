package session

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

func turn(role, content string) types.Message {
	return types.Message{Role: role, Content: content}
}

func TestMemorySessionStorePutGetListDelete(t *testing.T) {
	st := NewMemorySessionStore()
	if _, err := st.Get("missing"); err != ErrSessionNotFound {
		t.Fatalf("Get missing = %v, want ErrSessionNotFound", err)
	}

	s := Session{ID: "sess_a", OwnerKeyID: "k1", Model: "m", Status: StatusActive}
	if err := st.Put(s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get("sess_a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OwnerKeyID != "k1" || got.Model != "m" {
		t.Fatalf("Get returned %+v", got)
	}

	list, err := st.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v len %d, err %v", list, len(list), err)
	}

	// Delete is idempotent and missing-delete is not an error.
	if err := st.Delete("sess_a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete("sess_a"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	if _, err := st.Get("sess_a"); err != ErrSessionNotFound {
		t.Fatalf("Get after delete = %v", err)
	}
}

func TestMemoryHistoryStoreAppendGetDelete(t *testing.T) {
	hs := NewMemoryHistoryStore(0, 0) // unbounded
	got, err := hs.Get("none")
	if err != nil {
		t.Fatalf("Get empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty history = %v", got)
	}

	for i := 0; i < 3; i++ {
		if err := hs.Append("s", turn("user", "hi")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, _ = hs.Get("s")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	if err := hs.DeleteBySession("s"); err != nil {
		t.Fatalf("DeleteBySession: %v", err)
	}
	got, _ = hs.Get("s")
	if len(got) != 0 {
		t.Fatalf("after delete len = %d", len(got))
	}
}

// TestHistoryTurnCapTrimsOldest proves the turn-count cap trims oldest turns.
func TestHistoryTurnCapTrimsOldest(t *testing.T) {
	hs := NewMemoryHistoryStore(3, 0)
	for i := 0; i < 5; i++ {
		// content encodes the order so we can assert which survived.
		if err := hs.Append("s", turn("user", string(rune('0'+i)))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, _ := hs.Get("s")
	if len(got) != 3 {
		t.Fatalf("len = %d, want cap 3", len(got))
	}
	// The 3 most recent (2,3,4) must remain, in order.
	want := []string{"2", "3", "4"}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("turn[%d] = %q, want %q (full=%v)", i, got[i].Content, w, got)
		}
	}
}

// TestHistoryByteCapTrimsOldest proves the cumulative-byte cap trims oldest
// turns until the total fits.
func TestHistoryByteCapTrimsOldest(t *testing.T) {
	// Each "user"+content turn: role 4 bytes + content len. With content len 6
	// each turn is 10 bytes. Cap at 25 holds at most 2 turns (20), a 3rd (30)
	// trims the oldest.
	hs := NewMemoryHistoryStore(0, 25)
	hs.Append("s", turn("user", "aaaaaa")) // 10
	hs.Append("s", turn("user", "bbbbbb")) // 20
	got, _ := hs.Get("s")
	if len(got) != 2 {
		t.Fatalf("after 2 appends len = %d, want 2", len(got))
	}
	hs.Append("s", turn("user", "cccccc")) // would be 30 > 25 -> drop oldest
	got, _ = hs.Get("s")
	if len(got) != 2 {
		t.Fatalf("after 3rd append len = %d, want 2", len(got))
	}
	if got[0].Content != "bbbbbb" || got[1].Content != "cccccc" {
		t.Fatalf("trimmed wrong turns: %v", got)
	}
}

// TestHistoryByteCapKeepsOversizeSingleTurn proves a single turn larger than the
// byte cap is still retained (the documented exception), rather than wedging the
// session.
func TestHistoryByteCapKeepsOversizeSingleTurn(t *testing.T) {
	hs := NewMemoryHistoryStore(0, 8)
	big := turn("user", strings.Repeat("x", 100))
	if err := hs.Append("s", big); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := hs.Get("s")
	if len(got) != 1 || len(got[0].Content) != 100 {
		t.Fatalf("oversize single turn not retained: %v", got)
	}
}

// TestSessionStoreGetReturnsCopy proves Get/List hand callers deep copies so a
// returned session cannot mutate stored state. (Session has no reference fields
// today; this guards the clone seam against a future one.)
func TestSessionStoreGetReturnsCopy(t *testing.T) {
	st := NewMemorySessionStore()
	st.Put(Session{ID: "s", OwnerKeyID: "k", Model: "m"})
	got, _ := st.Get("s")
	got.Model = "mutated"
	again, _ := st.Get("s")
	if again.Model != "m" {
		t.Fatalf("stored session mutated through returned copy: %q", again.Model)
	}
}

// TestHistoryGetReturnsCopy proves mutating a turn returned from Get (including
// its ToolCalls slice) does not corrupt stored history.
func TestHistoryGetReturnsCopy(t *testing.T) {
	hs := NewMemoryHistoryStore(0, 0)
	hs.Append("s", types.Message{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "c1"}}})
	got, _ := hs.Get("s")
	got[0].Content = "mutated"
	got[0].ToolCalls[0].ID = "hacked"
	again, _ := hs.Get("s")
	if again[0].Content != "" || again[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("stored turn mutated through returned copy: %+v", again[0])
	}
}

// TestSessionHistoryCheckpointRoundTrip is the restart-survival proof: append
// turns, checkpoint both stores, build NEW stores, load, and see the data — with
// an already-expired session (and its history) dropped on load.
func TestSessionHistoryCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "sessions.json")
	histPath := filepath.Join(dir, "history.json")

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ss := NewMemorySessionStore()
	hs := NewMemoryHistoryStore(0, 0)

	// Live session, idle TTL 1h, last active at base.
	live := Session{ID: "sess_live", OwnerKeyID: "k", Model: "m", TTL: time.Hour,
		CreatedAt: base, LastActiveAt: base, Status: StatusActive}
	// Expired session: last active well in the past relative to load time.
	dead := Session{ID: "sess_dead", OwnerKeyID: "k", Model: "m", TTL: time.Hour,
		CreatedAt: base.Add(-2 * time.Hour), LastActiveAt: base.Add(-2 * time.Hour), Status: StatusActive}
	ss.Put(live)
	ss.Put(dead)
	hs.Append("sess_live", turn("user", "hello"))
	hs.Append("sess_dead", turn("user", "stale"))

	if err := ss.Checkpoint(sessPath, base); err != nil {
		t.Fatalf("session Checkpoint: %v", err)
	}
	if err := hs.Checkpoint(histPath); err != nil {
		t.Fatalf("history Checkpoint: %v", err)
	}

	// Simulate restart: fresh stores, load at base (so dead is already expired).
	ss2 := NewMemorySessionStore()
	hs2 := NewMemoryHistoryStore(0, 0)
	dropped, err := ss2.LoadCheckpoint(sessPath, base)
	if err != nil {
		t.Fatalf("session LoadCheckpoint: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "sess_dead" {
		t.Fatalf("dropped = %v, want [sess_dead]", dropped)
	}
	// Keep only the surviving sessions' history.
	keep := map[string]struct{}{"sess_live": {}}
	if err := hs2.LoadCheckpoint(histPath, keep); err != nil {
		t.Fatalf("history LoadCheckpoint: %v", err)
	}

	if _, err := ss2.Get("sess_live"); err != nil {
		t.Fatalf("live session lost across restart: %v", err)
	}
	if _, err := ss2.Get("sess_dead"); err != ErrSessionNotFound {
		t.Fatalf("expired session survived restart: %v", err)
	}
	got, _ := hs2.Get("sess_live")
	if len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("live history not restored: %v", got)
	}
	got, _ = hs2.Get("sess_dead")
	if len(got) != 0 {
		t.Fatalf("expired session history survived: %v", got)
	}
}

// TestLoadCheckpointMissingFileOK proves a missing checkpoint is treated as a
// fresh (empty) store, not an error.
func TestLoadCheckpointMissingFileOK(t *testing.T) {
	dir := t.TempDir()
	ss := NewMemorySessionStore()
	dropped, err := ss.LoadCheckpoint(filepath.Join(dir, "nope.json"), time.Now())
	if err != nil || dropped != nil {
		t.Fatalf("missing session checkpoint = (%v, %v)", dropped, err)
	}
	hs := NewMemoryHistoryStore(0, 0)
	if err := hs.LoadCheckpoint(filepath.Join(dir, "nope2.json"), nil); err != nil {
		t.Fatalf("missing history checkpoint: %v", err)
	}
}

// TestStoresConcurrentAccess exercises the stores under concurrency so the
// race detector (CI, amd64) can flag a data race. On this arm64 host -race is
// unsupported; the test still validates correctness without it.
func TestStoresConcurrentAccess(t *testing.T) {
	ss := NewMemorySessionStore()
	hs := NewMemoryHistoryStore(50, 0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess_" + string(rune('a'+i))
			for j := 0; j < 100; j++ {
				ss.Put(Session{ID: id, OwnerKeyID: "k", Model: "m"})
				_, _ = ss.Get(id)
				_, _ = ss.List()
				hs.Append(id, turn("user", "x"))
				_, _ = hs.Get(id)
			}
		}(i)
	}
	wg.Wait()
}
