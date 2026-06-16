package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// autoReply spins up a background goroutine that echoes a successful result for
// every job dispatched to the raw client, so a synchronous SubmitAuthorizedJob
// resolves. For each dispatched job it sends workerID on got, so the test can
// assert which worker received the turn. Stops when the stream closes or stop is
// closed.
func (r *rawClient) autoReply(t *testing.T, workerID string, stop <-chan struct{}, got chan<- string) {
	t.Helper()
	go func() {
		for {
			select {
			case <-stop:
				return
			case msg, ok := <-r.recvd:
				if !ok {
					return
				}
				if job := msg.GetJob(); job != nil {
					r.reply(t, job.GetId(), "ok:"+job.GetId())
					got <- workerID
				}
			}
		}
	}()
}

// heartbeatCapacity sends a single heartbeat advertising the given free VRAM and
// the llama3 model so the worker is a runnable, well-scored candidate.
func (r *rawClient) heartbeatCapacity(t *testing.T, id string, freeVRAM uint64) {
	t.Helper()
	r.heartbeat(t, types.Heartbeat{
		WorkerID:        id,
		FreeVRAM:        freeVRAM,
		TotalVRAM:       freeVRAM,
		AvailableModels: []types.Model{{Name: "llama3"}},
	})
}

// TestSessionAffinityHitThenRebindAfterLoss is the headline integration test for
// #34. It asserts (1) follow-up turns route to the worker a session is bound to
// (affinity HIT) even when a peer is a better raw fit, and (2) killing the bound
// worker makes the next turn rebind to the healthy peer (affinity MISS) and still
// succeed. The clock is injected and fast-forwarded so eviction needs no sleep.
func TestSessionAffinityHitThenRebindAfterLoss(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(time.Hour),
		session.WithSweepInterval(time.Hour), // no sweeping during the test
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}

	// Two workers. worker-b deliberately reports MORE free VRAM, so absent affinity
	// it is the strictly better fit and would win every turn; binding the session
	// to the worse-fit worker-a makes the affinity HIT observable (only the binding
	// keeps a turn on worker-a).
	wa := dialRaw(t, h, "worker-a", []types.Model{{Name: "llama3"}})
	defer wa.close()
	wb := dialRaw(t, h, "worker-b", []types.Model{{Name: "llama3"}})
	defer wb.close()

	stop := make(chan struct{})
	defer close(stop)
	dispatched := make(chan string, 16)
	wa.autoReply(t, "worker-a", stop, dispatched)
	wb.autoReply(t, "worker-b", stop, dispatched)

	// worker-a: 8 GiB free; worker-b: 64 GiB free (a better raw fit).
	wa.heartbeatCapacity(t, "worker-a", 8<<30)
	wb.heartbeatCapacity(t, "worker-b", 64<<30)
	waitFor(t, 2*time.Second, "both workers in fleet with capacity", func() bool {
		a, okA := fleetByID(h.srv, "worker-a")
		b, okB := fleetByID(h.srv, "worker-b")
		return okA && okB && a.FreeVRAM == 8<<30 && b.FreeVRAM == 64<<30
	})

	ctx := context.Background()
	sess, err := mgr.Create(ctx, key.ID, "llama3")
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	submit := func(jobID string) string {
		res, err := h.srv.SubmitAuthorizedJob(ctx, key,
			types.Job{ID: jobID, Model: "llama3", Prompt: "hi", SessionID: sess.ID})
		if err != nil {
			t.Fatalf("submit %s: %v", jobID, err)
		}
		if res.Err != nil {
			t.Fatalf("submit %s result err: %v", jobID, res.Err)
		}
		return res.Output
	}

	// Without affinity, worker-b (64 GiB) is the strictly better fit and would win
	// every turn. Bind the session to the WORSE-fit worker-a up front so the
	// affinity preference is the only thing that could route a turn there — making
	// the HIT observable rather than coincidental with the natural best fit.
	if _, err := mgr.Bind(ctx, sess.ID, key.ID, "worker-a"); err != nil {
		t.Fatalf("Bind to worker-a: %v", err)
	}

	// Turn 1 (AC1): the turn routes to the bound worker-a despite worker-b being
	// the better raw fit — an affinity HIT — and the binding is unchanged.
	submit("turn-1")
	first := <-dispatched
	if first != "worker-a" {
		t.Fatalf("turn-1 routed to %q, want bound worker-a (affinity hit over better fit)", first)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 1 || hs.Misses != 0 {
		t.Fatalf("after turn 1 affinity = %+v, want {Hits:1,Misses:0}", hs)
	}

	// Turn 2 (AC1): a follow-up on the same session stays on worker-a (second HIT).
	submit("turn-2")
	if second := <-dispatched; second != "worker-a" {
		t.Fatalf("turn-2 routed to %q, want bound worker-a (affinity hit)", second)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 2 || hs.Misses != 0 {
		t.Fatalf("after turn 2 affinity = %+v, want {Hits:2,Misses:0}", hs)
	}

	// Kill the bound worker-a: close its stream so removeWorker drops it from the
	// registry (a clean worker loss that does not touch worker-b). worker-b stays
	// online and selectable. No clock advance is needed, so worker-b cannot be
	// caught by the same staleness sweep.
	wa.close()
	waitFor(t, 2*time.Second, "worker-a gone, worker-b remains online", func() bool {
		_, goneOK := fleetByID(h.srv, "worker-a")
		b, survOK := fleetByID(h.srv, "worker-b")
		return !goneOK && survOK && b.Status == types.WorkerOnline
	})

	// Turn 3 (AC2): the bound worker is gone, so the turn must rebind to worker-b
	// and still succeed; an affinity MISS is recorded and BoundWorkerID updates.
	out := submit("turn-3")
	if out != "ok:turn-3" {
		t.Fatalf("turn-3 output = %q, want ok:turn-3", out)
	}
	if third := <-dispatched; third != "worker-b" {
		t.Fatalf("turn-3 routed to %q, want worker-b (rebind after loss)", third)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 2 || hs.Misses != 1 {
		t.Fatalf("after turn 3 affinity = %+v, want {Hits:2,Misses:1}", hs)
	}
	reboundSess, err := mgr.Get(ctx, sess.ID, key.ID)
	if err != nil {
		t.Fatalf("Get after rebind: %v", err)
	}
	if reboundSess.BoundWorkerID != "worker-b" {
		t.Fatalf("session rebound to %q, want worker-b", reboundSess.BoundWorkerID)
	}
}

// TestNoSessionLeavesAffinityUntouched asserts the default path is unchanged: a
// job with no SessionID never touches the session manager and records no affinity
// hit/miss, so behavior is byte-identical to today for non-session traffic.
func TestNoSessionLeavesAffinityUntouched(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(time.Hour),
		session.WithSweepInterval(time.Hour),
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}

	wa := dialRaw(t, h, "worker-a", []types.Model{{Name: "llama3"}})
	defer wa.close()
	stop := make(chan struct{})
	defer close(stop)
	dispatched := make(chan string, 8)
	wa.autoReply(t, "worker-a", stop, dispatched)
	wa.heartbeatCapacity(t, "worker-a", 8<<30)
	waitFor(t, 2*time.Second, "worker in fleet", func() bool {
		_, ok := fleetByID(h.srv, "worker-a")
		return ok
	})

	// Two turns with NO SessionID: dispatch works and no affinity is counted.
	for _, id := range []string{"j1", "j2"} {
		if _, err := h.srv.SubmitAuthorizedJob(context.Background(), key,
			types.Job{ID: id, Model: "llama3", Prompt: "x"}); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
		<-dispatched
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 0 || hs.Misses != 0 {
		t.Fatalf("affinity = %+v, want zero for session-less traffic", hs)
	}
}
