package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for TTL/expiry tests (no real sleep).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestManager builds a Manager over fresh in-memory stores with the injected
// clock and a short sweep interval so the sweeper reacts to the fake clock.
func newTestManager(t *testing.T, clk *fakeClock, opts ...Option) *Manager {
	t.Helper()
	base := []Option{
		WithClock(clk.now),
		WithTTL(time.Hour),
		WithSweepInterval(time.Millisecond),
	}
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(100, 1<<20), append(base, opts...)...)
	return m
}

func TestCreateReturnsStableUnguessableID(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)

	s1, err := m.Create(context.Background(), "k1", "llama")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(s1.ID, "sess_") || len(s1.ID) != len("sess_")+32 {
		t.Fatalf("id %q not sess_+32 hex", s1.ID)
	}
	if s1.Status != StatusActive {
		t.Fatalf("status = %q, want active", s1.Status)
	}
	if !s1.CreatedAt.Equal(clk.now()) || !s1.LastActiveAt.Equal(clk.now()) {
		t.Fatalf("timestamps not stamped to now: %+v", s1)
	}

	// The id is stable: fetching by it returns the same session.
	got, err := m.Get(context.Background(), s1.ID, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != s1.ID {
		t.Fatalf("Get id %q != created %q", got.ID, s1.ID)
	}

	// Ids are unique/unguessable across creations.
	s2, _ := m.Create(context.Background(), "k1", "llama")
	if s2.ID == s1.ID {
		t.Fatalf("duplicate id %q", s2.ID)
	}
}

func TestCreateEmptyOwnerRejected(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	if _, err := m.Create(context.Background(), "", "m"); err == nil {
		t.Fatalf("Create with empty owner should error")
	}
}

func TestCreateRandFailureSurfaced(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk, WithRandReader(func([]byte) (int, error) {
		return 0, errors.New("entropy depleted")
	}))
	if _, err := m.Create(context.Background(), "k", "m"); err == nil {
		t.Fatalf("Create should surface rand failure")
	}
}

func TestResumeTouchesLastActive(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	s, _ := m.Create(context.Background(), "k1", "m")
	created := s.LastActiveAt

	clk.advance(10 * time.Minute)
	resumed, err := m.Resume(context.Background(), s.ID, "k1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !resumed.LastActiveAt.After(created) {
		t.Fatalf("Resume did not advance LastActiveAt: %v <= %v", resumed.LastActiveAt, created)
	}
	if !resumed.LastActiveAt.Equal(clk.now()) {
		t.Fatalf("LastActiveAt = %v, want now %v", resumed.LastActiveAt, clk.now())
	}
}

// TestOwnerScopingIsolatesSessions proves a session created by key A is not
// readable, resumable, appendable, or deletable by key B (all return
// ErrSessionNotFound — no existence leak).
func TestOwnerScopingIsolatesSessions(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	s, _ := m.Create(context.Background(), "alice", "m")
	m.AppendTurn(context.Background(), s.ID, "alice", turn("user", "secret"))

	ctx := context.Background()
	if _, err := m.Get(ctx, s.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get as bob = %v, want ErrSessionNotFound", err)
	}
	if _, err := m.Resume(ctx, s.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Resume as bob = %v", err)
	}
	if _, err := m.History(ctx, s.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("History as bob = %v", err)
	}
	if err := m.AppendTurn(ctx, s.ID, "bob", turn("user", "x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AppendTurn as bob = %v", err)
	}
	if err := m.Delete(ctx, s.ID, "bob"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Delete as bob = %v", err)
	}

	// Alice's session and history are untouched.
	if _, err := m.Get(ctx, s.ID, "alice"); err != nil {
		t.Fatalf("alice lost her session after bob's probes: %v", err)
	}
	hist, _ := m.History(ctx, s.ID, "alice")
	if len(hist) != 1 || hist[0].Content != "secret" {
		t.Fatalf("alice history = %v", hist)
	}
}

func TestGetAndOpsOnMissingReturnNotFound(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	ctx := context.Background()
	if _, err := m.Get(ctx, "sess_nope", "k"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if err := m.AppendTurn(ctx, "sess_nope", "k", turn("user", "x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AppendTurn missing = %v", err)
	}
}

func TestDeleteRemovesSessionAndHistory(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	m.AppendTurn(ctx, s.ID, "k", turn("user", "hi"))

	if err := m.Delete(ctx, s.ID, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Get(ctx, s.ID, "k"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session still present after delete: %v", err)
	}
	// History is gone from the underlying store too.
	raw, _ := m.history.Get(s.ID)
	if len(raw) != 0 {
		t.Fatalf("history survived session delete: %v", raw)
	}
}

func TestAppendTurnEnforcesCaps(t *testing.T) {
	clk := newClock(time.Now())
	// Tiny turn cap so the boundary is easy to hit through the Manager.
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(2, 0),
		WithClock(clk.now), WithSweepInterval(time.Hour))
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	for _, c := range []string{"a", "b", "c"} {
		if err := m.AppendTurn(ctx, s.ID, "k", turn("user", c)); err != nil {
			t.Fatalf("AppendTurn %q: %v", c, err)
		}
	}
	hist, _ := m.History(ctx, s.ID, "k")
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want cap 2", len(hist))
	}
	if hist[0].Content != "b" || hist[1].Content != "c" {
		t.Fatalf("cap kept wrong turns: %v", hist)
	}
}

func TestAppendTurnTouchesLastActive(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	clk.advance(5 * time.Minute)
	if err := m.AppendTurn(ctx, s.ID, "k", turn("user", "hi")); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	got, _ := m.Get(ctx, s.ID, "k")
	if !got.LastActiveAt.Equal(clk.now()) {
		t.Fatalf("AppendTurn did not touch LastActiveAt: %v != %v", got.LastActiveAt, clk.now())
	}
}

// TestSweeperReapsExpiredSessionsAndHistory advances the injected clock past the
// TTL and asserts the sweeper deletes both the session and its history — no real
// sleep is used for the expiry decision.
func TestSweeperReapsExpiredSessionsAndHistory(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk) // TTL 1h, sweep every 1ms
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	m.AppendTurn(ctx, s.ID, "k", turn("user", "hi"))

	m.Start()
	defer m.Close()

	// Before expiry the session is present.
	if _, err := m.Get(ctx, s.ID, "k"); err != nil {
		t.Fatalf("session missing before expiry: %v", err)
	}

	// Jump the clock past the TTL; the wall-clock ticker (1ms) then drives a sweep.
	clk.advance(2 * time.Hour)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.sessions.Get(s.ID); errors.Is(err, ErrSessionNotFound) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.sessions.Get(s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("sweeper did not reap expired session")
	}
	hist, _ := m.history.Get(s.ID)
	if len(hist) != 0 {
		t.Fatalf("sweeper left history behind: %v", hist)
	}
}

// TestExpiredSessionNotReadableBeforeSweep proves a session past its TTL reads as
// ErrSessionNotFound even before the sweeper physically deletes it.
func TestExpiredSessionNotReadableBeforeSweep(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// No sweeper running (long interval); rely on read-time expiry.
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(100, 1<<20),
		WithClock(clk.now), WithTTL(time.Hour), WithSweepInterval(time.Hour))
	ctx := context.Background()
	s, _ := m.Create(ctx, "k", "m")
	clk.advance(2 * time.Hour)
	if _, err := m.Get(ctx, s.ID, "k"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired session readable: %v", err)
	}
}

// TestCloseIdempotentAndWithoutStart proves Close is safe to call without Start
// and more than once.
func TestCloseIdempotentAndWithoutStart(t *testing.T) {
	clk := newClock(time.Now())
	m := newTestManager(t, clk)
	if err := m.Close(); err != nil {
		t.Fatalf("Close without Start: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestNeverExpiresWithZeroTTL proves a session with a non-positive TTL is never
// reaped.
func TestNeverExpiresWithZeroTTL(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := NewManager(NewMemorySessionStore(), NewMemoryHistoryStore(100, 1<<20),
		WithClock(clk.now), WithTTL(time.Hour), WithSweepInterval(time.Millisecond))
	ctx := context.Background()
	// Put a TTL-0 session directly so we control the field.
	s := Session{ID: "sess_forever", OwnerKeyID: "k", Model: "m", TTL: 0,
		CreatedAt: clk.now(), LastActiveAt: clk.now(), Status: StatusActive}
	m.sessions.Put(s)
	m.Start()
	defer m.Close()
	clk.advance(100 * time.Hour)
	time.Sleep(20 * time.Millisecond)
	if _, err := m.Get(ctx, "sess_forever", "k"); err != nil {
		t.Fatalf("zero-TTL session was reaped: %v", err)
	}
}

// TestManagerConcurrentLifecycle stresses the Manager under concurrent
// create/resume/append/delete so the race detector (CI amd64) can flag races.
func TestManagerConcurrentLifecycle(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newTestManager(t, clk)
	m.Start()
	defer m.Close()
	ctx := context.Background()

	var created int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s, err := m.Create(ctx, "k", "m")
				if err != nil {
					continue
				}
				atomic.AddInt64(&created, 1)
				_, _ = m.Resume(ctx, s.ID, "k")
				_ = m.AppendTurn(ctx, s.ID, "k", turn("user", "x"))
				_, _ = m.History(ctx, s.ID, "k")
				_ = m.Delete(ctx, s.ID, "k")
			}
		}()
	}
	wg.Wait()
	if created == 0 {
		t.Fatalf("no sessions created under concurrency")
	}
}
