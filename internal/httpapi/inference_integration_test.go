package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/httpapi"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
	"github.com/jaypetez/agent-gpu/internal/worker"
)

// scriptedExecutor is a worker.Executor that streams a fixed sequence of chunks
// for any job, then returns a final result. It lets the inference integration
// tests exercise the full server<->worker stream (per-token deltas, tool calls,
// finish_reason, the token split) without a real Ollama. A job's tools are
// echoed onto the response when wantToolCall is set so function-calling
// round-trips can be asserted end-to-end.
type scriptedExecutor struct {
	models []types.Model
	// deltas are emitted one per JobChunk before the terminal result.
	deltas []string
	// toolCall, when non-nil, is returned on the result with finish_reason
	// "tool_calls" (mirroring a model that decided to call a function).
	toolCall *types.ToolCall
	promptTokens,
	completionTokens uint64
	// mu guards lastJob, written from the worker's Execute goroutine and read
	// from the test goroutine. The HTTP response completing implies the job
	// finished, but the mutex makes the access explicitly race-free under -race.
	mu sync.Mutex
	// lastJob captures the most recent job for assertions on what the HTTP layer
	// threaded through (messages, tools, prompt).
	lastJob *types.Job
}

func (e *scriptedExecutor) Execute(_ context.Context, job types.Job, emit func(types.JobChunk)) types.JobResult {
	j := job
	e.mu.Lock()
	e.lastJob = &j
	e.mu.Unlock()

	var sb strings.Builder
	for _, d := range e.deltas {
		sb.WriteString(d)
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, Delta: d})
		}
	}

	res := types.JobResult{
		JobID:            job.ID,
		Output:           sb.String(),
		PromptTokens:     e.promptTokens,
		CompletionTokens: e.completionTokens,
		Tokens:           e.promptTokens + e.completionTokens,
		FinishReason:     "stop",
	}
	if e.toolCall != nil {
		res.ToolCalls = []types.ToolCall{*e.toolCall}
		res.FinishReason = "tool_calls"
		if emit != nil {
			emit(types.JobChunk{JobID: job.ID, ToolCalls: res.ToolCalls})
		}
	}
	return res
}

func (e *scriptedExecutor) ListModels(context.Context) ([]types.Model, error) { return e.models, nil }
func (e *scriptedExecutor) Pull(context.Context, string) error                { return nil }

// job returns a copy of the last job the worker executed, for assertions.
func (e *scriptedExecutor) job() *types.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastJob
}

// inferenceHarness wires a real control-plane gRPC server, a worker running the
// given executor over bufconn, and the HTTP API together, returning the HTTP
// base URL and an admin token. It blocks until the worker's model is visible in
// the catalog so dispatch will find a worker.
type inferenceHarness struct {
	url   string
	token string
}

func newInferenceHarness(t *testing.T, exec *scriptedExecutor, model string) inferenceHarness {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

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

	exec.models = []types.Model{{Name: model, Digest: "sha256:test"}}
	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)
	w := worker.New(worker.Config{
		ServerAddr:        "bufconn",
		WorkerID:          "worker-1",
		Models:            exec.models,
		Executor:          exec,
		Logger:            discard,
		HeartbeatInterval: 15 * time.Millisecond,
		Backoff:           worker.Backoff{Base: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2.0},
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})
	go func() { _ = w.Run(wctx) }()

	h := inferenceHarness{url: ts.URL, token: token}
	// Wait until the model is visible so a dispatch will find a worker.
	waitFor(t, 2*time.Second, "model in catalog", func() bool {
		return len(fetchModels(t, h.url, h.token)) == 1
	})
	return h
}

func (h inferenceHarness) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestChatCompletionNonStreaming proves a non-streaming chat request round-trips
// end-to-end: the message history threads to the worker, the assistant content
// comes back in the OpenAI chat.completion shape, and usage reflects the token
// split. (AC1, AC5 non-streaming.)
func TestChatCompletionNonStreaming(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"Hello", ", ", "world"}, promptTokens: 7, completionTokens: 3}
	h := newInferenceHarness(t, exec, "llama3")

	resp := h.post(t, "/v1/chat/completions", `{
		"model":"llama3",
		"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]
	}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
			TotalTokens      uint64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Errorf("id = %q, want chatcmpl- prefix", out.ID)
	}
	if out.Created == 0 {
		t.Errorf("created = 0, want a unix timestamp")
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	c := out.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "Hello, world" {
		t.Errorf("message = %+v, want assistant/'Hello, world'", c.Message)
	}
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", c.FinishReason)
	}
	if out.Usage.PromptTokens != 7 || out.Usage.CompletionTokens != 3 || out.Usage.TotalTokens != 10 {
		t.Errorf("usage = %+v, want 7/3/10", out.Usage)
	}

	// The full message history threaded to the worker.
	job := exec.job()
	if job == nil || len(job.Messages) != 2 {
		t.Fatalf("worker job messages = %+v, want 2 messages", job)
	}
	if job.Messages[0].Role != "system" || job.Messages[1].Content != "hi" {
		t.Errorf("threaded messages wrong: %+v", job.Messages)
	}
}

// TestChatCompletionStreaming proves a stream=true chat request yields
// incremental chunks in OpenAI SSE format: the first frame carries the
// assistant role, subsequent frames carry content deltas, the terminal frame
// sets finish_reason, and the stream ends with [DONE]. (AC2, AC5 streaming.)
func TestChatCompletionStreaming(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"Hel", "lo", "!"}, promptTokens: 2, completionTokens: 3}
	h := newInferenceHarness(t, exec, "llama3")

	resp := h.post(t, "/v1/chat/completions", `{
		"model":"llama3",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", cc)
	}

	frames, done := readSSE(t, resp.Body)
	if !done {
		t.Fatalf("stream did not end with [DONE]")
	}
	if len(frames) < 2 {
		t.Fatalf("got %d data frames, want >=2", len(frames))
	}

	// First frame announces the assistant role.
	var first struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(frames[0], &first); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if first.Object != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk", first.Object)
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first delta role = %q, want assistant", first.Choices[0].Delta.Role)
	}

	// Concatenated content deltas reconstruct the full message; the last frame
	// carries the finish_reason.
	var content strings.Builder
	var finish string
	for _, f := range frames {
		var fr struct {
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
		content.WriteString(fr.Choices[0].Delta.Content)
		if fr.Choices[0].FinishReason != nil {
			finish = *fr.Choices[0].FinishReason
		}
	}
	if content.String() != "Hello!" {
		t.Errorf("streamed content = %q, want Hello!", content.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
}

// TestChatCompletionToolCalling proves a function-calling request round-trips:
// the tool definition reaches the worker, the assistant tool_calls come back,
// and finish_reason is "tool_calls". (AC3.)
func TestChatCompletionToolCalling(t *testing.T) {
	exec := &scriptedExecutor{
		toolCall: &types.ToolCall{
			ID:           "call_abc",
			Type:         "function",
			FunctionName: "get_weather",
			Arguments:    `{"city":"paris"}`,
		},
		promptTokens:     11,
		completionTokens: 4,
	}
	h := newInferenceHarness(t, exec, "llama3")

	resp := h.post(t, "/v1/chat/completions", `{
		"model":"llama3",
		"messages":[{"role":"user","content":"weather in paris?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
	}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := out.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", c.FinishReason)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(c.Message.ToolCalls))
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v, want get_weather/call_abc/function", tc)
	}
	if tc.Function.Arguments != `{"city":"paris"}` {
		t.Errorf("arguments = %q, want the paris object", tc.Function.Arguments)
	}

	// The tool definition reached the worker with its JSON-schema parameters
	// intact.
	job := exec.job()
	if job == nil || len(job.Tools) != 1 {
		t.Fatalf("worker job tools = %+v, want 1 tool", job)
	}
	tool := job.Tools[0]
	if tool.Function.Name != "get_weather" {
		t.Errorf("threaded tool name = %q, want get_weather", tool.Function.Name)
	}
	if !strings.Contains(tool.Function.Parameters, `"city"`) {
		t.Errorf("threaded tool params lost schema: %q", tool.Function.Parameters)
	}
}

// TestCompletionNonStreaming proves the legacy /v1/completions surface maps the
// prompt onto Job.Prompt and returns a text_completion. (AC1.)
func TestCompletionNonStreaming(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"world"}, completionTokens: 1}
	h := newInferenceHarness(t, exec, "llama3")

	resp := h.post(t, "/v1/completions", `{"model":"llama3","prompt":"hello"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Text         string `json:"text"`
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "text_completion" {
		t.Errorf("object = %q, want text_completion", out.Object)
	}
	if out.Choices[0].Text != "world" {
		t.Errorf("text = %q, want world", out.Choices[0].Text)
	}
	if job := exec.job(); job == nil || job.Prompt != "hello" {
		t.Errorf("worker prompt = %+v, want hello", job)
	}
}

// TestCompletionStreaming proves /v1/completions streams text_completion SSE
// frames terminated by [DONE]. (AC2, AC5 streaming.)
func TestCompletionStreaming(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"wo", "rld"}, completionTokens: 2}
	h := newInferenceHarness(t, exec, "llama3")

	resp := h.post(t, "/v1/completions", `{"model":"llama3","prompt":"hello","stream":true}`)
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
	var text strings.Builder
	var finish string
	for _, f := range frames {
		var fr struct {
			Object  string `json:"object"`
			Choices []struct {
				Text         string  `json:"text"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(f, &fr); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if fr.Object != "text_completion" {
			t.Errorf("object = %q, want text_completion", fr.Object)
		}
		text.WriteString(fr.Choices[0].Text)
		if fr.Choices[0].FinishReason != nil {
			finish = *fr.Choices[0].FinishReason
		}
	}
	if text.String() != "world" {
		t.Errorf("streamed text = %q, want world", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
}

// TestChatCompletionUnauthorized proves a missing key is rejected with 401
// before any dispatch.
func TestChatCompletionUnauthorized(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"x"}}
	h := newInferenceHarness(t, exec, "llama3")

	req, err := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions",
		strings.NewReader(`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestChatCompletionForbiddenModel proves a key not permitted for the model is
// rejected with 403 (authz.ErrForbidden mapped) before any worker is touched.
func TestChatCompletionForbiddenModel(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	az := authz.NewAuthorizer(authz.WithLogger(discard))
	grpcSrv := server.New(server.WithLogger(discard), server.WithStore(st), server.WithAuthorizer(az))
	grpcSrv.Start()
	defer func() { _ = grpcSrv.Close() }()

	authSvc := auth.NewService(st)
	httpSrv := httpapi.NewServer(grpcSrv, authSvc, az, discard, "")
	ts := httptest.NewServer(httpSrv.Handler())
	defer ts.Close()

	// A non-admin user key with an allow-list that does NOT include "secret".
	token, _, err := authSvc.CreateWithPermissions(context.Background(), "user",
		auth.Permissions{Roles: []string{authz.RoleUser}, AllowModels: []string{"llama3"}})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"secret","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestChatCompletionMalformedBody proves a malformed JSON body is rejected with
// 400 (invalid_request_error).
func TestChatCompletionMalformedBody(t *testing.T) {
	exec := &scriptedExecutor{deltas: []string{"x"}}
	h := newInferenceHarness(t, exec, "llama3")
	resp := h.post(t, "/v1/chat/completions", `{not json`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// readSSE reads SSE frames from r, returning the JSON payloads of each `data:`
// line (excluding the [DONE] sentinel) and whether the [DONE] sentinel was seen.
func readSSE(t *testing.T, r io.Reader) (frames [][]byte, done bool) {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if string(payload) == "[DONE]" {
			done = true
			continue
		}
		frames = append(frames, append([]byte(nil), payload...))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read sse: %v", err)
	}
	return frames, done
}
