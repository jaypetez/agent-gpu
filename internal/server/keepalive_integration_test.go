package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// warmHarness stands up a server wired with a session manager (the only way the
// dispatcher derives a warm window) plus a single raw worker advertising llama3,
// and returns both. ttl is the session idle TTL; warmMax caps the warm window.
func warmHarness(t *testing.T, ttl, warmMax time.Duration) (*harness, *session.Manager, *rawClient) {
	t.Helper()
	clk := newTestClock(time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(100, 1<<20),
		session.WithClock(clk.now),
		session.WithTTL(ttl),
		session.WithSweepInterval(time.Hour), // no sweeping mid-test
	)
	h := newHarnessWith(t,
		server.WithClock(clk.now),
		server.WithSessionManager(mgr),
		server.WithModelWarmMax(warmMax),
		server.WithHeartbeatTimeout(time.Minute),
		server.WithEvictScanInterval(5*time.Millisecond),
		server.WithPlaceScanInterval(5*time.Millisecond),
	)
	w := dialRaw(t, h, "worker-a", []types.Model{{Name: "llama3"}})
	w.heartbeatCapacity(t, "worker-a", 8<<30)
	waitFor(t, 2*time.Second, "worker in fleet", func() bool {
		_, ok := fleetByID(h.srv, "worker-a")
		return ok
	})
	return h, mgr, w
}

// submitAndCaptureJob submits job in a background goroutine (so the synchronous
// dispatch can be intercepted), captures the proto Job the worker actually
// received, replies so the submit resolves, and returns the captured Job. It is
// the seam for asserting what crossed the wire — here, keep_alive_seconds (#35).
func submitAndCaptureJob(t *testing.T, h *harness, w *rawClient, key store.APIKey, job types.Job) *agentgpuv1.Job {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := h.srv.SubmitAuthorizedJob(context.Background(), key, job)
		done <- err
	}()
	dispatched := w.awaitJob(t)
	w.reply(t, dispatched.GetId(), "ok")
	if err := <-done; err != nil {
		t.Fatalf("submit %s: %v", job.ID, err)
	}
	return dispatched
}

// TestSessionBoundJobCarriesWarmKeepAlive is the headline server-side test for
// #35: a session-bound turn dispatches with keep_alive_seconds set from the
// session's idle TTL (below the cap), so the worker keeps the model warm across
// the conversation.
func TestSessionBoundJobCarriesWarmKeepAlive(t *testing.T) {
	h, mgr, w := warmHarness(t, 20*time.Minute, time.Hour)
	defer w.close()
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}
	sess, err := mgr.Create(context.Background(), key.ID, "llama3")
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	job := submitAndCaptureJob(t, h, w, key,
		types.Job{ID: "turn-1", Model: "llama3", Prompt: "hi", SessionID: sess.ID})
	if got := job.GetKeepAliveSeconds(); got != 1200 {
		t.Fatalf("keep_alive_seconds = %d, want 1200 (20m TTL)", got)
	}
}

// TestSessionBoundJobWarmWindowCapped asserts the warm window is bounded by the
// configured cap even when the session TTL is larger — an abandoned long-TTL
// session cannot pin VRAM for longer than the cap (AC: abandoned sessions do not
// pin VRAM indefinitely).
func TestSessionBoundJobWarmWindowCapped(t *testing.T) {
	h, mgr, w := warmHarness(t, 4*time.Hour, 30*time.Minute)
	defer w.close()
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}
	sess, err := mgr.Create(context.Background(), key.ID, "llama3")
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	job := submitAndCaptureJob(t, h, w, key,
		types.Job{ID: "turn-1", Model: "llama3", Prompt: "hi", SessionID: sess.ID})
	if got := job.GetKeepAliveSeconds(); got != 1800 {
		t.Fatalf("keep_alive_seconds = %d, want 1800 (30m cap over 4h TTL)", got)
	}
}

// TestSessionlessJobHasNoKeepAlive asserts a job with no session dispatches with
// keep_alive_seconds 0, so the worker omits keep_alive and Ollama's own default
// unload window applies — byte-identical to pre-#35 behavior.
func TestSessionlessJobHasNoKeepAlive(t *testing.T) {
	h, _, w := warmHarness(t, 20*time.Minute, time.Hour)
	defer w.close()
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	key := store.APIKey{ID: "k1", Roles: []string{"admin"}}
	job := submitAndCaptureJob(t, h, w, key,
		types.Job{ID: "j1", Model: "llama3", Prompt: "hi"}) // no SessionID
	if got := job.GetKeepAliveSeconds(); got != 0 {
		t.Fatalf("keep_alive_seconds = %d, want 0 for a session-less job", got)
	}
}

// TestUnloadSessionModelSendsControlMessage asserts UnloadSessionModel delivers an
// UnloadModel control message to the named worker (the explicit-release path,
// #35), carrying the model to evict.
func TestUnloadSessionModelSendsControlMessage(t *testing.T) {
	h, _, w := warmHarness(t, 20*time.Minute, time.Hour)
	defer w.close()
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	h.srv.UnloadSessionModel(context.Background(), "worker-a", "llama3")

	select {
	case msg, ok := <-w.recvd:
		if !ok {
			t.Fatal("stream closed before unload message")
		}
		um := msg.GetUnloadModel()
		if um == nil {
			t.Fatalf("expected UnloadModel, got %T", msg.GetPayload())
		}
		if um.GetModel() != "llama3" {
			t.Fatalf("unload model = %q, want llama3", um.GetModel())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UnloadModel message")
	}
}

// TestUnloadSessionModelUnknownWorkerNoop asserts UnloadSessionModel is a safe
// no-op when the target worker is not connected (drained/stale/never-bound): no
// panic, no error — the keep_alive idle timer remains the release path.
func TestUnloadSessionModelUnknownWorkerNoop(t *testing.T) {
	h, _, w := warmHarness(t, 20*time.Minute, time.Hour)
	defer w.close()
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// Must not block or panic; nothing is delivered to the connected worker.
	h.srv.UnloadSessionModel(context.Background(), "worker-ghost", "llama3")

	select {
	case msg, ok := <-w.recvd:
		if ok && msg.GetUnloadModel() != nil {
			t.Fatal("unload delivered to the wrong worker")
		}
	case <-time.After(100 * time.Millisecond):
		// No message — the expected outcome.
	}
}
