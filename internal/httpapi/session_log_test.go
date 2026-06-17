package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/session"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// jobResultOK is a minimal successful inference result (with a token split) for
// the fakeEngine the session-log tests drive.
func jobResultOK(output string) types.JobResult {
	return types.JobResult{Output: output, PromptTokens: 1, CompletionTokens: 1, Tokens: 2, FinishReason: "stop"}
}

// syncBuf is a mutex-guarded buffer so the JSON log handler (written from the
// request goroutine) and the test goroutine reading it are race-free under -race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sessionLogServer builds an httpapi.Server wired to a real auth service, a real
// session.Manager, and the given fake engine, logging structured JSON into buf so
// a test can assert the session_id correlation on the log lines. It returns the
// server, the manager, and an admin token.
func sessionLogServer(t *testing.T, buf *syncBuf, eng inferenceEngine) (*Server, *session.Manager, string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	az := authz.NewAuthorizer(authz.WithLogger(logger))
	mgr := session.NewManager(
		session.NewMemorySessionStore(),
		session.NewMemoryHistoryStore(0, 0),
		session.WithLogger(logger),
		session.WithTTL(time.Hour),
	)
	t.Cleanup(func() { _ = mgr.Close() })
	s := &Server{
		fleet:      &fakeFleet{},
		engine:     eng,
		auth:       authSvc,
		authz:      az,
		sessionMgr: mgr,
		log:        logger,
	}
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return s, mgr, token
}

// logLineWhere scans newline-delimited JSON log output for a line whose named
// field equals want, returning the decoded record and whether it was found.
func logLineWhere(t *testing.T, out, field, want string) (map[string]any, bool) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if v, ok := rec[field].(string); ok && v == want {
			return rec, true
		}
	}
	return nil, false
}

// TestStatefulChatLogsSessionAndRequestID is the headline session-traceability AC
// test for #38: a stateful chat turn emits a log line carrying BOTH the session_id
// (the conversation, across turns) and the request_id (this turn). Filtering logs
// by session_id therefore surfaces a whole conversation end-to-end, and by
// request_id one turn.
func TestStatefulChatLogsSessionAndRequestID(t *testing.T) {
	buf := &syncBuf{}
	s, mgr, token := sessionLogServer(t, buf, &fakeEngine{res: jobResultOK("hi")})

	sess, err := mgr.Create(context.Background(), keyIDForToken(t, s, token), "llama3")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A stateful turn references the session by the body session_id field, with a
	// known inbound X-Request-Id so we can assert both ids land on the same line.
	const reqID = "req-trace-xyz"
	body := `{"model":"llama3","session_id":"` + sess.ID + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The turn line carries the session_id...
	line, ok := logLineWhere(t, buf.String(), "session_id", sess.ID)
	if !ok {
		t.Fatalf("no log line with session_id == %q\nlog:\n%s", sess.ID, buf.String())
	}
	// ...AND the same line carries the request_id (the turn correlation), so a
	// conversation is traceable by session_id and a single turn by request_id.
	if line["request_id"] != reqID {
		t.Errorf("session line request_id = %v, want %q (both ids on one line)", line["request_id"], reqID)
	}
	if line["msg"] != "session chat turn" {
		t.Errorf("session line msg = %v, want 'session chat turn'", line["msg"])
	}
}

// TestStatelessChatHasNoSessionID proves a stateless chat request (no session id
// anywhere) logs NO session_id attribute — the correlation is added only for
// session-aware turns, so a stateless request is unchanged.
func TestStatelessChatHasNoSessionID(t *testing.T) {
	buf := &syncBuf{}
	s, _, token := sessionLogServer(t, buf, &fakeEngine{res: jobResultOK("hi")})

	body := `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if strings.Contains(buf.String(), "session_id") {
		t.Errorf("stateless request logged a session_id attribute:\n%s", buf.String())
	}
}

// TestSessionCreateLogsSessionID proves the session CRUD create handler logs the
// new session_id alongside request_id, so a conversation is traceable from its
// first moment.
func TestSessionCreateLogsSessionID(t *testing.T) {
	buf := &syncBuf{}
	s, _, token := sessionLogServer(t, buf, &fakeEngine{res: jobResultOK("hi")})

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"model":"llama3"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	line, ok := logLineWhere(t, buf.String(), "session_id", out.ID)
	if !ok {
		t.Fatalf("create logged no session_id == %q\nlog:\n%s", out.ID, buf.String())
	}
	if _, hasReq := line["request_id"].(string); !hasReq {
		t.Errorf("create session line missing request_id: %v", line)
	}
	if line["msg"] != "session created" {
		t.Errorf("create line msg = %v, want 'session created'", line["msg"])
	}
}

// keyIDForToken resolves the API key id for a bearer token via the server's auth
// service, so a test can own a session under the same key it authenticates with.
func keyIDForToken(t *testing.T, s *Server, token string) string {
	t.Helper()
	key, err := s.auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return key.ID
}
