package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/types"
	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// Tests for the admin model-management dispatch methods added in #93:
// AdminPullModel and AdminUnloadModel. They assert the correct ServerMessage
// reaches the worker over a real control-plane stream and that an unknown worker
// is reported as ErrWorkerNotFound (so the HTTP layer can 404). Authorization is
// the HTTP scope gate, so these methods deliberately apply no model authz — a
// fact covered by the httpapi scope tests.

// awaitServerMessage blocks until the worker receives a server message matching
// want, or fails the test. It drains intervening messages (e.g. the RegisterAck
// is consumed by dialRaw already, but heartleak/keepalive messages could appear).
func awaitServerMessage(t *testing.T, rc *rawClient, want func(*agentgpuv1.ServerMessage) bool) *agentgpuv1.ServerMessage {
	t.Helper()
	for {
		select {
		case msg, ok := <-rc.recvd:
			if !ok {
				t.Fatal("stream closed before expected server message")
			}
			if want(msg) {
				return msg
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for expected server message")
		}
	}
}

// TestAdminPullModelDispatches proves AdminPullModel sends a PullModel message
// naming the model to the target worker, and returns ErrWorkerNotFound for an
// unknown worker.
func TestAdminPullModelDispatches(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// Unknown worker → ErrWorkerNotFound (no dispatch).
	if err := h.srv.AdminPullModel(context.Background(), "missing", "llama3"); !errors.Is(err, server.ErrWorkerNotFound) {
		t.Fatalf("AdminPullModel(missing) = %v, want ErrWorkerNotFound", err)
	}

	rc := dialRaw(t, h, "pull-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	if err := h.srv.AdminPullModel(context.Background(), "pull-worker", "mistral"); err != nil {
		t.Fatalf("AdminPullModel: %v", err)
	}

	msg := awaitServerMessage(t, rc, func(m *agentgpuv1.ServerMessage) bool {
		return m.GetPullModel() != nil
	})
	if got := msg.GetPullModel().GetModel(); got != "mistral" {
		t.Fatalf("PullModel model = %q, want mistral", got)
	}
}

// TestAdminUnloadModelDispatches proves AdminUnloadModel sends an UnloadModel
// message naming the model to the target worker, and returns ErrWorkerNotFound
// for an unknown worker (so DELETE can 404). A model that is not loaded is a
// worker-side no-op, so a connected-worker dispatch always returns nil.
func TestAdminUnloadModelDispatches(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	// Unknown worker → ErrWorkerNotFound (so the admin DELETE maps to 404).
	if err := h.srv.AdminUnloadModel(context.Background(), "missing", "llama3"); !errors.Is(err, server.ErrWorkerNotFound) {
		t.Fatalf("AdminUnloadModel(missing) = %v, want ErrWorkerNotFound", err)
	}

	rc := dialRaw(t, h, "unload-worker", []types.Model{{Name: "llama3"}})
	defer rc.close()
	waitFor(t, 2*time.Second, "worker registered", func() bool {
		return h.srv.WorkerCount() == 1
	})

	// A connected worker dispatch returns nil regardless of whether the model is
	// actually resident (the executor treats not-found as success).
	if err := h.srv.AdminUnloadModel(context.Background(), "unload-worker", "llama3"); err != nil {
		t.Fatalf("AdminUnloadModel: %v", err)
	}

	msg := awaitServerMessage(t, rc, func(m *agentgpuv1.ServerMessage) bool {
		return m.GetUnloadModel() != nil
	})
	if got := msg.GetUnloadModel().GetModel(); got != "llama3" {
		t.Fatalf("UnloadModel model = %q, want llama3", got)
	}
}

// TestWorkerByID proves the per-worker snapshot accessor returns the live
// snapshot for a connected worker and false for an unknown id (#93 detail seam).
func TestWorkerByID(t *testing.T) {
	h := newHarnessWith(t, server.WithHeartbeatTimeout(time.Minute))
	defer h.close()
	defer func() { _ = h.srv.Close() }()

	if _, ok := h.srv.WorkerByID("missing"); ok {
		t.Fatal("WorkerByID(missing) ok = true, want false")
	}

	rc := dialRaw(t, h, "by-id", []types.Model{{Name: "llama3"}})
	defer rc.close()
	rc.heartbeat(t, types.Heartbeat{WorkerID: "by-id", Load: 7})
	waitFor(t, 2*time.Second, "heartbeat surfaced", func() bool {
		w, ok := h.srv.WorkerByID("by-id")
		return ok && w.Load == 7
	})

	w, ok := h.srv.WorkerByID("by-id")
	if !ok || w.ID != "by-id" || w.Status != types.WorkerOnline {
		t.Fatalf("WorkerByID = %+v ok=%v, want by-id online", w, ok)
	}
	if len(w.Models) != 1 || w.Models[0].Name != "llama3" {
		t.Fatalf("WorkerByID models = %+v, want llama3", w.Models)
	}
	if w.RegisteredAt.IsZero() {
		t.Fatal("WorkerByID RegisteredAt should be set")
	}
}
