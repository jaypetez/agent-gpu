package testutil_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/testutil"
	"github.com/jaypetez/agent-gpu/internal/types"
)

func TestJobDefaults(t *testing.T) {
	t.Parallel()
	j := testutil.Job()
	if err := j.Validate(); err != nil {
		t.Fatalf("default Job() is invalid: %v", err)
	}
	if j.ID != testutil.DefaultJobID || j.Model != testutil.DefaultModel || j.Prompt != testutil.DefaultPrompt {
		t.Fatalf("unexpected defaults: %+v", j)
	}
}

func TestJobOptions(t *testing.T) {
	t.Parallel()
	tool := types.Tool{Type: "function", Function: types.ToolFunction{Name: "f"}}
	j := testutil.Job(
		testutil.WithJobID("job-x"),
		testutil.WithModel("mistral"),
		testutil.WithPrompt(""),
		testutil.WithMessages(testutil.UserMessage("hi"), testutil.AssistantMessage("yo")),
		testutil.WithTools(tool),
		testutil.WithSessionID("sess_1"),
	)
	if j.ID != "job-x" || j.Model != "mistral" || j.Prompt != "" {
		t.Fatalf("scalar options not applied: %+v", j)
	}
	if len(j.Messages) != 2 || j.Messages[0].Role != "user" || j.Messages[1].Role != "assistant" {
		t.Fatalf("WithMessages not applied: %+v", j.Messages)
	}
	if len(j.Tools) != 1 || j.Tools[0].Function.Name != "f" {
		t.Fatalf("WithTools not applied: %+v", j.Tools)
	}
	if j.SessionID != "sess_1" {
		t.Fatalf("WithSessionID not applied: %q", j.SessionID)
	}

	// Later option overrides earlier.
	j2 := testutil.Job(testutil.WithModel("a"), testutil.WithModel("b"))
	if j2.Model != "b" {
		t.Fatalf("later option should win: got %q", j2.Model)
	}
}

func TestWorkerBuilder(t *testing.T) {
	t.Parallel()
	w := testutil.Worker()
	if w.ID != "worker-1" || w.Status != types.WorkerOnline {
		t.Fatalf("unexpected worker defaults: %+v", w)
	}

	const gib = uint64(1) << 30
	w = testutil.Worker(
		testutil.WithWorkerID("w2"),
		testutil.WithWorkerModels("llama3", "mistral"),
		testutil.WithFreeVRAM(8*gib),
		testutil.WithTotalVRAM(16*gib),
		testutil.WithLoad(42),
		testutil.WithActiveJobs(3),
		testutil.WithGPUType("nvidia"),
		testutil.WithStatus(types.WorkerDraining),
	)
	if w.ID != "w2" || w.Status != types.WorkerDraining {
		t.Fatalf("worker scalar options not applied: %+v", w)
	}
	if len(w.Models) != 2 || w.Models[0].Name != "llama3" || w.Models[1].Name != "mistral" {
		t.Fatalf("WithWorkerModels not applied: %+v", w.Models)
	}
	if w.FreeVRAM != 8*gib || w.TotalVRAM != 16*gib || w.Load != 42 || w.ActiveJobs != 3 || w.GPUType != "nvidia" {
		t.Fatalf("worker capacity options not applied: %+v", w)
	}

	w = testutil.Worker(testutil.WithWorkerModelObjects(types.Model{Name: "x", Digest: "sha256:d"}))
	if len(w.Models) != 1 || w.Models[0].Digest != "sha256:d" {
		t.Fatalf("WithWorkerModelObjects not applied: %+v", w.Models)
	}

	seen := time.Unix(1700000000, 0).UTC()
	w = testutil.Worker(testutil.WithLastSeen(seen))
	if !w.LastSeen.Equal(seen) {
		t.Fatalf("WithLastSeen not applied: %v", w.LastSeen)
	}
}

func TestHeartbeatBuilder(t *testing.T) {
	t.Parallel()
	hb := testutil.Heartbeat()
	if hb.WorkerID != "worker-1" {
		t.Fatalf("unexpected heartbeat default id: %q", hb.WorkerID)
	}
	hb = testutil.Heartbeat(
		testutil.WithHeartbeatWorkerID("w9"),
		testutil.WithHeartbeatModels("llama3"),
		testutil.WithHeartbeatVRAM(100, 60),
		testutil.WithHeartbeatLoad(7),
		testutil.WithHeartbeatActiveJobs(2),
		testutil.WithHeartbeatGPUType("apple"),
	)
	if hb.WorkerID != "w9" || hb.TotalVRAM != 100 || hb.FreeVRAM != 60 || hb.Load != 7 || hb.ActiveJobs != 2 || hb.GPUType != "apple" {
		t.Fatalf("heartbeat options not applied: %+v", hb)
	}
	if len(hb.AvailableModels) != 1 || hb.AvailableModels[0].Name != "llama3" {
		t.Fatalf("WithHeartbeatModels not applied: %+v", hb.AvailableModels)
	}
	hb = testutil.Heartbeat(testutil.WithHeartbeatModelObjects(types.Model{Name: "x", Digest: "sha256:d"}))
	if len(hb.AvailableModels) != 1 || hb.AvailableModels[0].Digest != "sha256:d" {
		t.Fatalf("WithHeartbeatModelObjects not applied: %+v", hb.AvailableModels)
	}
}

func TestKeyBuilder(t *testing.T) {
	t.Parallel()
	k := testutil.Key()
	if k.Name != "test" || k.Prefix != auth.Prefix {
		t.Fatalf("unexpected key defaults: %+v", k)
	}
	k = testutil.Key(
		testutil.WithKeyName("svc"),
		testutil.WithRoles(authz.RoleUser),
		testutil.WithAllowModels("llama3"),
		testutil.WithDenyModels("secret"),
		testutil.WithLimits(store.Limits{RPM: 5, TPM: 100}),
	)
	if k.Name != "svc" || len(k.Roles) != 1 || k.Roles[0] != authz.RoleUser {
		t.Fatalf("key role options not applied: %+v", k)
	}
	if len(k.AllowModels) != 1 || k.AllowModels[0] != "llama3" || len(k.DenyModels) != 1 || k.DenyModels[0] != "secret" {
		t.Fatalf("key allow/deny not applied: %+v", k)
	}
	if k.Limits == nil || k.Limits.RPM != 5 || k.Limits.TPM != 100 {
		t.Fatalf("WithLimits not applied: %+v", k.Limits)
	}

	// WithRPM sets only RPM, leaving other dimensions unlimited.
	k = testutil.Key(testutil.WithRPM(3))
	if k.Limits == nil || k.Limits.RPM != 3 || k.Limits.TPM != 0 {
		t.Fatalf("WithRPM not applied cleanly: %+v", k.Limits)
	}
}

func TestMintKey(t *testing.T) {
	t.Parallel()
	svc := auth.NewService(store.NewMemory())
	key, token := testutil.MintKey(t, svc,
		testutil.WithKeyName("user"),
		testutil.WithRoles(authz.RoleUser),
		testutil.WithAllowModels("llama3"),
		testutil.WithRPM(2),
	)
	if token == "" {
		t.Fatal("MintKey returned empty token")
	}
	if key.Limits == nil || key.Limits.RPM != 2 {
		t.Fatalf("minted key limits = %+v, want RPM 2", key.Limits)
	}

	// The token authenticates and resolves to the same key id with the granted role.
	got, err := svc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("minted token failed to authenticate: %v", err)
	}
	if got.ID != key.ID {
		t.Fatalf("authenticated id = %q, want %q", got.ID, key.ID)
	}
	if len(got.Roles) != 1 || got.Roles[0] != authz.RoleUser {
		t.Fatalf("authenticated roles = %v, want [user]", got.Roles)
	}
}

func TestMintToken(t *testing.T) {
	t.Parallel()
	svc := auth.NewService(store.NewMemory())
	token := testutil.MintToken(t, svc, testutil.WithRoles(authz.RoleAdmin))
	if _, err := svc.Authenticate(context.Background(), token); err != nil {
		t.Fatalf("MintToken token failed to authenticate: %v", err)
	}
}

func TestFakeExecutorEchoDefault(t *testing.T) {
	t.Parallel()
	e := testutil.NewFakeExecutor()
	var deltas []string
	res := e.Execute(context.Background(), testutil.Job(testutil.WithPrompt("hi")), func(c types.JobChunk) {
		deltas = append(deltas, c.Delta)
	})
	if res.Output != "echo: hi" {
		t.Fatalf("echo output = %q, want %q", res.Output, "echo: hi")
	}
	if len(deltas) != 1 || deltas[0] != "echo: hi" {
		t.Fatalf("echo deltas = %v", deltas)
	}
	// "echo: hi" has 2 whitespace-separated tokens.
	if res.CompletionTokens != 2 || res.Tokens != 2 {
		t.Fatalf("echo tokens = %d/%d, want 2/2", res.CompletionTokens, res.Tokens)
	}
	if e.Handled() != 1 {
		t.Fatalf("Handled = %d, want 1", e.Handled())
	}
	if lj := e.LastJob(); lj == nil || lj.Prompt != "hi" {
		t.Fatalf("LastJob = %+v, want prompt hi", lj)
	}
}

func TestFakeExecutorDeltas(t *testing.T) {
	t.Parallel()
	e := testutil.NewFakeExecutor(testutil.WithDeltas("a", "b", "c"), testutil.WithTokens(2, 3))
	var got []string
	res := e.Execute(context.Background(), testutil.Job(), func(c types.JobChunk) { got = append(got, c.Delta) })
	if res.Output != "abc" {
		t.Fatalf("output = %q, want abc", res.Output)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("deltas = %v", got)
	}
	if res.PromptTokens != 2 || res.CompletionTokens != 3 || res.Tokens != 5 {
		t.Fatalf("token split = %d/%d/%d, want 2/3/5", res.PromptTokens, res.CompletionTokens, res.Tokens)
	}
}

func TestFakeExecutorReplyVariants(t *testing.T) {
	t.Parallel()
	// Single-delta reply.
	e := testutil.NewFakeExecutor(testutil.WithReply("hello"))
	var single []string
	res := e.Execute(context.Background(), testutil.Job(), func(c types.JobChunk) { single = append(single, c.Delta) })
	if res.Output != "hello" || len(single) != 1 || single[0] != "hello" {
		t.Fatalf("single-delta reply wrong: out=%q deltas=%v", res.Output, single)
	}
	if res.PromptTokens != 1 || res.CompletionTokens != 1 {
		t.Fatalf("reply default token split = %d/%d, want 1/1", res.PromptTokens, res.CompletionTokens)
	}

	// Rune-by-rune reply.
	e = testutil.NewFakeExecutor(testutil.WithReplyPerRune("hi"))
	var runes []string
	res = e.Execute(context.Background(), testutil.Job(), func(c types.JobChunk) { runes = append(runes, c.Delta) })
	if res.Output != "hi" || len(runes) != 2 || runes[0] != "h" || runes[1] != "i" {
		t.Fatalf("per-rune reply wrong: out=%q deltas=%v", res.Output, runes)
	}
}

func TestFakeExecutorToolCall(t *testing.T) {
	t.Parallel()
	tc := types.ToolCall{ID: "call_1", Type: "function", FunctionName: "get_weather", Arguments: `{"city":"paris"}`}
	e := testutil.NewFakeExecutor(testutil.WithToolCall(tc))
	var toolDeltas int
	res := e.Execute(context.Background(), testutil.Job(), func(c types.JobChunk) {
		if len(c.ToolCalls) > 0 {
			toolDeltas++
		}
	})
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", res.FinishReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].FunctionName != "get_weather" {
		t.Fatalf("result tool calls = %+v", res.ToolCalls)
	}
	if toolDeltas != 1 {
		t.Fatalf("emitted tool-call deltas = %d, want 1", toolDeltas)
	}
}

func TestFakeExecutorBlockReleased(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	emitted := make(chan struct{})
	e := testutil.NewFakeExecutor(
		testutil.WithDeltas("par", "tial"),
		testutil.WithBlock(release),
		testutil.WithEmitSignal(emitted),
	)

	done := make(chan types.JobResult, 1)
	go func() {
		done <- e.Execute(context.Background(), testutil.Job(), func(types.JobChunk) {})
	}()

	// It emits, signals, then blocks: Execute has not returned yet.
	<-emitted
	select {
	case <-done:
		t.Fatal("Execute returned before release")
	default:
	}

	close(release)
	res := <-done
	if res.Output != "partial" {
		t.Fatalf("output = %q, want partial", res.Output)
	}
}

func TestFakeExecutorBlockContextCancel(t *testing.T) {
	t.Parallel()
	emitted := make(chan struct{})
	// No release channel: only context cancellation unblocks (disconnect case).
	e := testutil.NewFakeExecutor(testutil.WithDeltas("x"), testutil.WithBlock(nil), testutil.WithEmitSignal(emitted))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Execute(ctx, testutil.Job(), func(types.JobChunk) {})
		close(done)
	}()

	<-emitted
	cancel()
	<-done // returns once the context is cancelled
	if e.Handled() != 1 {
		t.Fatalf("Handled = %d, want 1", e.Handled())
	}
}

func TestFakeExecutorListModelsAndPull(t *testing.T) {
	t.Parallel()
	e := testutil.NewFakeExecutor(testutil.WithExecModels("llama3", "mistral"))
	models, err := e.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].Name != "llama3" {
		t.Fatalf("ListModels = %+v", models)
	}

	// WithExecModelObjects passes full model values (name + digest) through.
	e2 := testutil.NewFakeExecutor(testutil.WithExecModelObjects(types.Model{Name: "x", Digest: "sha256:d"}))
	m2, err := e2.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(m2) != 1 || m2[0].Digest != "sha256:d" {
		t.Fatalf("WithExecModelObjects = %+v", m2)
	}

	if err := e.Pull(context.Background(), "llama3"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := e.Pull(context.Background(), "mistral"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulls := e.Pulls(); len(pulls) != 2 || pulls[0] != "llama3" || pulls[1] != "mistral" {
		t.Fatalf("Pulls = %v, want [llama3 mistral]", pulls)
	}
}

func TestFakeExecutorErrors(t *testing.T) {
	t.Parallel()
	pullErr := errors.New("pull boom")
	listErr := errors.New("list boom")
	e := testutil.NewFakeExecutor(testutil.WithPullErr(pullErr), testutil.WithListModelsErr(listErr))

	if _, err := e.ListModels(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("ListModels err = %v, want %v", err, listErr)
	}
	if err := e.Pull(context.Background(), "m"); !errors.Is(err, pullErr) {
		t.Fatalf("Pull err = %v, want %v", err, pullErr)
	}
	// Pull still records the attempt even when it errors.
	if pulls := e.Pulls(); len(pulls) != 1 || pulls[0] != "m" {
		t.Fatalf("Pulls = %v, want [m]", pulls)
	}
}

func TestFakeExecutorWithEcho(t *testing.T) {
	t.Parallel()
	// WithEcho forces echo even after a reply was set.
	e := testutil.NewFakeExecutor(testutil.WithReply("ignored"), testutil.WithEcho())
	res := e.Execute(context.Background(), testutil.Job(testutil.WithPrompt("p")), func(types.JobChunk) {})
	if res.Output != "echo: p" {
		t.Fatalf("WithEcho output = %q, want echo: p", res.Output)
	}
	if lj := e.LastJob(); lj == nil {
		t.Fatal("LastJob nil")
	}
}

// TestFakeExecutorNoLastJob proves LastJob is nil before any Execute call.
func TestFakeExecutorNoLastJob(t *testing.T) {
	t.Parallel()
	e := testutil.NewFakeExecutor()
	if lj := e.LastJob(); lj != nil {
		t.Fatalf("LastJob before Execute = %+v, want nil", lj)
	}
	if pulls := e.Pulls(); pulls != nil {
		t.Fatalf("Pulls before Pull = %v, want nil", pulls)
	}
}
