package httpapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// newTestServer builds an httpapi.Server bound to an ephemeral local port with
// throwaway dependencies, suitable for exercising the start/stop lifecycle.
func newTestServer(t *testing.T) *httpapi.Server {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	grpcSrv := server.New(server.WithLogger(discard), server.WithStore(st), server.WithAuthorizer(az))
	authSvc := auth.NewService(st)
	// Port 0 lets the kernel pick a free port so parallel runs don't collide.
	return httpapi.NewServer(grpcSrv, authSvc, az, nil, nil, discard, "127.0.0.1:0")
}

// TestShutdownBeforeListenAndServe proves that Shutdown is a clean no-op when
// ListenAndServe was never called: the *http.Server is constructed in NewServer
// so the pointer is always non-nil and stable. This is the immediate-cancel
// path — the caller cancels before the serve goroutine ever runs — and it must
// not panic or block.
func TestShutdownBeforeListenAndServe(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before ListenAndServe: %v", err)
	}
}

// TestImmediateCancelDrainsListener covers the shutdown-ordering guarantee: even
// if Shutdown is called before the serve goroutine has been scheduled, the
// listener is poisoned so a subsequent ListenAndServe returns ErrServerClosed
// immediately rather than serving traffic. There is therefore no window where
// the HTTP listener is left undrained while the rest of shutdown proceeds.
func TestImmediateCancelDrainsListener(t *testing.T) {
	s := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// A serve goroutine started after Shutdown must observe ErrServerClosed and
	// never begin accepting connections.
	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe() }()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe after Shutdown = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after prior Shutdown; listener left undrained")
	}
}

// TestListenAndServeThenShutdown exercises the normal lifecycle: the server
// starts serving, then Shutdown drains it and ListenAndServe returns
// ErrServerClosed (the documented clean-stop signal the cmd treats as normal).
func TestListenAndServeThenShutdown(t *testing.T) {
	s := newTestServer(t)

	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe() }()

	// Give the goroutine a moment to bind and begin serving.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}
