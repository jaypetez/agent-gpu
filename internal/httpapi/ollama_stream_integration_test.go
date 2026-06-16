package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/ollama"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// stubOllama returns an httptest server speaking just enough of the Ollama HTTP
// API for a worker's real OllamaExecutor: /api/version, /api/tags (so the model
// surfaces in the catalog), and a streaming NDJSON /api/chat that emits one
// token object per element of toks followed by a terminal done object.
func stubOllama(t *testing.T, model string, toks []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.5.0"}`)
		case "/api/tags":
			fmt.Fprintf(w, `{"models":[{"name":%q}]}`, model)
		case "/api/chat":
			fl, _ := w.(http.Flusher)
			for _, tok := range toks {
				fmt.Fprintf(w, `{"message":{"content":%q},"done":false}`+"\n", tok)
				if fl != nil {
					fl.Flush()
				}
			}
			fmt.Fprint(w, `{"done":true,"eval_count":3,"prompt_eval_count":2}`+"\n")
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestChatCompletionStreamingThroughOllama wires the full stack — HTTP API →
// gRPC control plane → a worker whose real OllamaExecutor talks to a stubbed,
// streaming Ollama — and drives stream=true through the public chat endpoint.
// It proves the SSE frames a client receives accumulate to the stub's token
// output and the stream terminates with [DONE], exercising the real
// OllamaExecutor path end-to-end at the client layer (AC1, AC5 streaming).
func TestChatCompletionStreamingThroughOllama(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	stub := stubOllama(t, "llama3", []string{"po", "ng", "!"})

	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	grpcSrv := server.New(
		server.WithLogger(discard),
		server.WithStore(st),
		server.WithAuthorizer(az),
		server.WithHeartbeatTimeout(2*time.Second),
		server.WithEvictScanInterval(50*time.Millisecond),
	)
	grpcSrv.Start()
	t.Cleanup(func() { _ = grpcSrv.Close() })

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	grpcSrv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	t.Cleanup(ts.Close)

	token, _, err := authSvc.CreateWithPermissions(context.Background(), "admin",
		auth.Permissions{Roles: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// A worker running the real OllamaExecutor pointed at the stub. Its model
	// list is sourced from /api/tags via heartbeats.
	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "ollama-worker",
		Logger:            discard,
		HeartbeatInterval: 15 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		Executor:          worker.NewOllamaExecutor(ollama.New(stub.URL)),
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})
	go func() { _ = w.Run(wctx) }()

	// Wait until the model from /api/tags is visible so dispatch finds a worker.
	waitFor(t, 2*time.Second, "ollama model in catalog", func() bool {
		return len(fetchModels(t, ts.URL, token)) == 1
	})

	resp := postStream(t, ts.URL, token, `{
		"model":"llama3",
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	frames, done := readSSE(t, resp.Body)
	if !done {
		t.Fatalf("stream did not end with [DONE]")
	}
	if len(frames) < 2 {
		t.Fatalf("got %d data frames, want >=2 (role + deltas)", len(frames))
	}

	var content strings.Builder
	var finish string
	for _, f := range frames {
		var fr struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(f, &fr); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if fr.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", fr.Object)
		}
		if len(fr.Choices) > 0 {
			content.WriteString(fr.Choices[0].Delta.Content)
			if fr.Choices[0].FinishReason != nil {
				finish = *fr.Choices[0].FinishReason
			}
		}
	}
	if content.String() != "pong!" {
		t.Errorf("streamed content = %q, want pong!", content.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
}

// postStream issues a streaming chat request and returns the live response so
// the caller can read SSE frames from the body.
func postStream(t *testing.T, baseURL, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}
