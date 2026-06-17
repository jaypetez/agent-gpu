package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/testutil"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// dispatchRecord is one observed server->worker job dispatch, carrying the
// worker that received it and the keep_alive window stamped onto it. It is the
// end-to-end evidence the e2e session tests assert on: same worker across turns
// (affinity), a different worker after loss (rebind), and a non-zero keep_alive
// carried on every session-bound turn (#35).
type dispatchRecord struct {
	worker    string
	keepAlive int64
}

// captureReply is autoReply with keep_alive capture: for every job dispatched
// to the raw worker it replies "ok:<jobID>" (so the synchronous
// SubmitAuthorizedJob resolves) and forwards a dispatchRecord on got so the test
// can assert which worker handled the turn AND what keep_alive crossed the wire.
// It exists because the shared autoReply forwards only the worker id; the rest
// of the multi-worker bufconn harness (dialRaw, heartbeatCapacity, reply,
// waitFor, fleetByID, the injected clock) is reused unchanged.
func (r *rawClient) captureReply(t *testing.T, workerID string, stop <-chan struct{}, got chan<- dispatchRecord) {
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
					got <- dispatchRecord{worker: workerID, keepAlive: job.GetKeepAliveSeconds()}
				}
			}
		}
	}()
}

// TestSessionE2EMultiTurnAffinityRebindAndKeepAlive drives a multi-turn stateful
// conversation through the full stack — the control-plane server, the scheduler,
// and two in-process workers over bufconn — and asserts the three load-bearing
// session guarantees end to end:
//
//   - Affinity stickiness: consecutive turns of the same session land on the SAME
//     bound worker, even though the peer is the strictly better raw fit (so only
//     the binding could route the turn there).
//   - Rebind after worker loss: when the bound worker disappears, the next turn
//     succeeds on a DIFFERENT worker with no client-visible failure, and the
//     session is re-bound to the new worker.
//   - keep_alive carried across turns: every session-bound dispatch stamps the
//     warm window (#35) derived from the session TTL, so the model stays resident
//     across the conversation.
//
// It also exercises the stateful conversation history: each turn appends a
// user+assistant pair through the Manager, and the accumulated history is read
// back through the owner-scoped path. The injected clock means no real time
// passes; waitFor polls for the asynchronous fleet effects.
func TestSessionE2EMultiTurnAffinityRebindAndKeepAlive(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	const ttl = 20 * time.Minute
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(ttl),
		session.WithSweepInterval(time.Hour), // no sweeping during this test
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithModelWarmMax(time.Hour), // cap above TTL, so the window == TTL
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}

	// Two workers advertising the same model. worker-b deliberately reports MORE
	// free VRAM, so absent affinity it is the strictly better fit and would win
	// every turn; binding the session to the worse-fit worker-a is what makes the
	// affinity HIT observable rather than coincidental with the natural best fit.
	wa := dialRaw(t, h, "worker-a", []types.Model{{Name: "llama3"}})
	defer wa.close()
	wb := dialRaw(t, h, "worker-b", []types.Model{{Name: "llama3"}})
	defer wb.close()

	stop := make(chan struct{})
	defer close(stop)
	dispatched := make(chan dispatchRecord, 16)
	wa.captureReply(t, "worker-a", stop, dispatched)
	wb.captureReply(t, "worker-b", stop, dispatched)

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
	// Bind to the WORSE-fit worker-a up front so an affinity preference is the only
	// thing that could route a turn there.
	if _, err := mgr.Bind(ctx, sess.ID, key.ID, "worker-a"); err != nil {
		t.Fatalf("Bind to worker-a: %v", err)
	}

	// turn drives one conversation turn end to end: it submits a session-bound job
	// (which the server routes via affinity and replies to through captureReply),
	// records the user prompt and assistant reply into the session history through
	// the Manager (the stateful-conversation path), and returns the observed
	// dispatch so the caller can assert on routing and keep_alive.
	turn := func(jobID, prompt string) dispatchRecord {
		t.Helper()
		res, err := h.srv.SubmitAuthorizedJob(ctx, key,
			types.Job{ID: jobID, Model: "llama3", Prompt: prompt, SessionID: sess.ID})
		if err != nil {
			t.Fatalf("submit %s: %v", jobID, err)
		}
		if res.Err != nil {
			t.Fatalf("submit %s result err: %v", jobID, res.Err)
		}
		if res.Output != "ok:"+jobID {
			t.Fatalf("submit %s output = %q, want ok:%s", jobID, res.Output, jobID)
		}
		// Persist the turn (user prompt + assistant reply) so the conversation has
		// real, growing history — exercising the stateful path and giving the expiry
		// test's sibling something to reap.
		if err := mgr.AppendTurns(ctx, sess.ID, key.ID,
			testutil.UserMessage(prompt), testutil.AssistantMessage(res.Output)); err != nil {
			t.Fatalf("append turn %s: %v", jobID, err)
		}
		return <-dispatched
	}

	// keep_alive_seconds the server must stamp on every bound turn: min(TTL, cap).
	// With a 20m TTL under a 1h cap the warm window is the full TTL, 1200s.
	const wantKeepAlive = int64(ttl / time.Second)

	// Turn 1 (AC1): routes to the bound worker-a despite worker-b being the better
	// raw fit — an affinity HIT — and carries the warm keep_alive window.
	rec := turn("turn-1", "hello")
	if rec.worker != "worker-a" {
		t.Fatalf("turn-1 routed to %q, want bound worker-a (affinity hit over better fit)", rec.worker)
	}
	if rec.keepAlive != wantKeepAlive {
		t.Fatalf("turn-1 keep_alive = %d, want %d (TTL warm window)", rec.keepAlive, wantKeepAlive)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 1 || hs.Misses != 0 {
		t.Fatalf("after turn 1 affinity = %+v, want {Hits:1,Misses:0}", hs)
	}

	// Turn 2 (AC1): a follow-up on the same session stays on worker-a (second HIT),
	// and keep_alive is re-sent so the model stays warm across the conversation.
	rec = turn("turn-2", "how are you")
	if rec.worker != "worker-a" {
		t.Fatalf("turn-2 routed to %q, want bound worker-a (affinity hit)", rec.worker)
	}
	if rec.keepAlive != wantKeepAlive {
		t.Fatalf("turn-2 keep_alive = %d, want %d (re-sent each turn)", rec.keepAlive, wantKeepAlive)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 2 || hs.Misses != 0 {
		t.Fatalf("after turn 2 affinity = %+v, want {Hits:2,Misses:0}", hs)
	}

	// Two turns recorded so far: 2 messages each = 4 stored turns.
	hist, err := mgr.History(ctx, sess.ID, key.ID)
	if err != nil {
		t.Fatalf("History after 2 turns: %v", err)
	}
	if len(hist) != 4 {
		t.Fatalf("history length = %d, want 4 (2 turns x user+assistant)", len(hist))
	}

	// Kill the bound worker-a: closing its stream makes removeWorker drop it from
	// the registry (a clean worker loss that does not touch worker-b). No clock
	// advance is needed, so worker-b cannot be caught by a staleness sweep.
	wa.close()
	waitFor(t, 2*time.Second, "worker-a gone, worker-b remains online", func() bool {
		_, goneOK := fleetByID(h.srv, "worker-a")
		b, survOK := fleetByID(h.srv, "worker-b")
		return !goneOK && survOK && b.Status == types.WorkerOnline
	})

	// Turn 3 (AC2): the bound worker is gone, so the turn must rebind to worker-b
	// and STILL SUCCEED — no client-visible failure (turn would t.Fatalf on error).
	// An affinity MISS/rebind is recorded and the session re-binds to worker-b.
	rec = turn("turn-3", "still there?")
	if rec.worker != "worker-b" {
		t.Fatalf("turn-3 routed to %q, want worker-b (rebind after loss)", rec.worker)
	}
	if rec.keepAlive != wantKeepAlive {
		t.Fatalf("turn-3 keep_alive = %d, want %d (carried after rebind)", rec.keepAlive, wantKeepAlive)
	}
	if hs := h.srv.AffinityStats(); hs.Hits != 2 || hs.Misses != 1 || hs.Rebinds != 1 {
		t.Fatalf("after turn 3 affinity = %+v, want {Hits:2,Misses:1,Rebinds:1}", hs)
	}
	rebound, err := mgr.Get(ctx, sess.ID, key.ID)
	if err != nil {
		t.Fatalf("Get after rebind: %v", err)
	}
	if rebound.BoundWorkerID != "worker-b" {
		t.Fatalf("session rebound to %q, want worker-b", rebound.BoundWorkerID)
	}

	// The conversation history survived the rebind and grew with turn 3: 3 turns
	// x 2 messages = 6 stored turns. The rebind is transparent to the conversation.
	hist, err = mgr.History(ctx, sess.ID, key.ID)
	if err != nil {
		t.Fatalf("History after rebind: %v", err)
	}
	if len(hist) != 6 {
		t.Fatalf("history length = %d, want 6 (3 turns x user+assistant)", len(hist))
	}
}

// TestSessionE2EExpiryReapsSessionAndHistory drives one stateful turn through the
// full stack to bind a session and accumulate history, then fast-forwards the
// injected clock past the session's idle TTL and lets the REAL idle-expiry
// sweeper reap it. It asserts the AC3 invariant end to end: an expired session is
// gone from the Manager (Get -> ErrSessionNotFound) AND its conversation history
// is physically deleted from the history store (not merely inaccessible through
// the owner-gated path). No real time passes — the clock is advanced and the
// short sweep interval lets the sweeper observe it; waitFor polls for the reap.
func TestSessionE2EExpiryReapsSessionAndHistory(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	const ttl = 15 * time.Minute
	// Hold the concrete history store so the test can prove the history bytes are
	// physically purged on expiry, independent of the owner-scoped Manager.History
	// path (which would fail once the session row is gone regardless).
	histStore := session.NewMemoryHistoryStore(100, 1<<20)
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		histStore,
		session.WithClock(clk.now),
		session.WithTTL(ttl),
		session.WithSweepInterval(2*time.Millisecond), // react promptly to the fast-forwarded clock
	)
	// Run the real sweeper; Close stops it (idempotent, safe even if never Started).
	mgr.Start()
	defer func() { _ = mgr.Close() }()

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
	dispatched := make(chan dispatchRecord, 8)
	wa.captureReply(t, "worker-a", stop, dispatched)
	wa.heartbeatCapacity(t, "worker-a", 8<<30)
	waitFor(t, 2*time.Second, "worker in fleet", func() bool {
		_, ok := fleetByID(h.srv, "worker-a")
		return ok
	})

	ctx := context.Background()
	sess, err := mgr.Create(ctx, key.ID, "llama3")
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// One real turn through the stack: dispatch (binds the session to worker-a on
	// the first turn) plus a persisted user+assistant pair, so there is genuine
	// history to reap.
	res, err := h.srv.SubmitAuthorizedJob(ctx, key,
		types.Job{ID: "turn-1", Model: "llama3", Prompt: "hi", SessionID: sess.ID})
	if err != nil {
		t.Fatalf("submit turn-1: %v", err)
	}
	if res.Output != "ok:turn-1" {
		t.Fatalf("turn-1 output = %q, want ok:turn-1", res.Output)
	}
	<-dispatched
	if err := mgr.AppendTurns(ctx, sess.ID, key.ID,
		testutil.UserMessage("hi"), testutil.AssistantMessage(res.Output)); err != nil {
		t.Fatalf("append turn-1: %v", err)
	}

	// Sanity: the session is live, bound, and has history before expiry.
	if got, err := mgr.Get(ctx, sess.ID, key.ID); err != nil {
		t.Fatalf("Get before expiry: %v", err)
	} else if got.BoundWorkerID != "worker-a" {
		t.Fatalf("session bound to %q before expiry, want worker-a", got.BoundWorkerID)
	}
	if hist, err := histStore.Get(sess.ID); err != nil || len(hist) != 2 {
		t.Fatalf("history before expiry = %d turns (err=%v), want 2", len(hist), err)
	}

	// Fast-forward past the idle TTL; the running sweeper must reap the idle session
	// (and its history) on its next tick. No real sleep — only the clock moves.
	clk.advance(ttl + time.Minute)

	// AC3a: the session is reaped — Get returns ErrSessionNotFound.
	waitFor(t, 2*time.Second, "expired session reaped from manager", func() bool {
		_, err := mgr.Get(ctx, sess.ID, key.ID)
		return errors.Is(err, session.ErrSessionNotFound)
	})

	// AC3b: the history is physically gone from the store, proving the sweeper's
	// DeleteBySession ran — not just that the owner-gated path now refuses access.
	waitFor(t, 2*time.Second, "expired session history purged from store", func() bool {
		hist, err := histStore.Get(sess.ID)
		return err == nil && len(hist) == 0
	})

	// And the owner-scoped History path also refuses the reaped session, so no
	// caller can read its turns post-expiry.
	if _, err := mgr.History(ctx, sess.ID, key.ID); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("History after expiry = %v, want ErrSessionNotFound", err)
	}
}

// TestSessionE2EQuotaCapRejectsExtraSession asserts the per-key concurrent-session
// cap (#37) is enforced through the Manager wired into the live server stack: with
// a cap of 1, a second concurrent Create for the same owner is rejected with
// ErrSessionLimitExceeded (the typed seam the HTTP layer maps to 429), while a
// different owner is unaffected and the slot frees on Delete. This is the
// session-quota rejection AC, kept in the same e2e file as the must-have flows.
func TestSessionE2EQuotaCapRejectsExtraSession(t *testing.T) {
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(time.Hour),
		session.WithSweepInterval(time.Hour),
		session.WithMaxSessionsPerKey(1),
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
	)
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	ctx := context.Background()

	// First session for the owner is accepted (at the cap of 1).
	first, err := mgr.Create(ctx, "k1", "llama3")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Second concurrent session for the SAME owner is rejected at the cap.
	if _, err := mgr.Create(ctx, "k1", "llama3"); !errors.Is(err, session.ErrSessionLimitExceeded) {
		t.Fatalf("second Create = %v, want ErrSessionLimitExceeded", err)
	}
	// A different owner is unaffected by k1's cap.
	if _, err := mgr.Create(ctx, "k2", "llama3"); err != nil {
		t.Fatalf("Create for other owner: %v", err)
	}
	// Deleting the first frees k1's slot, so a fresh create succeeds again.
	if err := mgr.Delete(ctx, first.ID, "k1"); err != nil {
		t.Fatalf("Delete first: %v", err)
	}
	if _, err := mgr.Create(ctx, "k1", "llama3"); err != nil {
		t.Fatalf("Create after freeing slot: %v", err)
	}
}
