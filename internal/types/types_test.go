package types

import (
	"errors"
	"reflect"
	"testing"

	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

func TestJobValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		job     Job
		wantErr bool
	}{
		{"valid", Job{ID: "j1", Model: "llama3", Prompt: "hi"}, false},
		{"valid no prompt", Job{ID: "j1", Model: "llama3"}, false},
		{"missing id", Job{Model: "llama3"}, true},
		{"missing model", Job{ID: "j1"}, true},
		{"empty", Job{}, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.job.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidJob) {
					t.Fatalf("error %v should wrap ErrInvalidJob", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestJobErrorError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  *JobError
		want string
	}{
		{"nil", nil, ""},
		{"code and message", &JobError{Code: "model_not_found", Message: "no model"}, "model_not_found: no model"},
		{"message only", &JobError{Message: "boom"}, "boom"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobErrorIsError(t *testing.T) {
	t.Parallel()
	var err error = &JobError{Code: "x", Message: "y"}
	if err.Error() == "" {
		t.Fatal("JobError should satisfy the error interface with non-empty text")
	}
}

func TestJobRoundTrip(t *testing.T) {
	t.Parallel()
	in := Job{ID: "j1", Model: "llama3", Prompt: "hello"}
	got := JobFromProto(in.Proto())
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, in)
	}
}

// TestJobKeepAliveRoundTrip proves the model-warmth hint (#35) crosses the wire:
// KeepAliveSeconds survives Proto/JobFromProto for a positive (warm), zero
// (unset), and negative (keep-forever) value, so the server's warm window reaches
// the worker.
func TestJobKeepAliveRoundTrip(t *testing.T) {
	t.Parallel()
	for _, secs := range []int64{0, 900, -1} {
		in := Job{ID: "j1", Model: "llama3", Prompt: "hi", KeepAliveSeconds: secs}
		got := JobFromProto(in.Proto())
		if got.KeepAliveSeconds != secs {
			t.Fatalf("KeepAliveSeconds round trip: got %d want %d", got.KeepAliveSeconds, secs)
		}
		// The full value still round-trips intact alongside the new field.
		if !reflect.DeepEqual(got, in) {
			t.Fatalf("round trip mismatch: got %+v want %+v", got, in)
		}
	}
}

// TestJobSessionIDNotOnWire proves SessionID is a server-side-only routing hint
// (#34): Proto drops it and JobFromProto never sets it, so the worker contract is
// unchanged. (Contrast KeepAliveSeconds, which is carried on purpose — #35.)
func TestJobSessionIDNotOnWire(t *testing.T) {
	t.Parallel()
	in := Job{ID: "j1", Model: "llama3", Prompt: "hi", SessionID: "sess_abc", KeepAliveSeconds: 600}
	got := JobFromProto(in.Proto())
	if got.SessionID != "" {
		t.Fatalf("SessionID crossed the wire: got %q, want empty", got.SessionID)
	}
	// Everything except SessionID survives; keep_alive in particular is carried.
	want := in
	want.SessionID = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestModelRoundTrip(t *testing.T) {
	t.Parallel()
	in := Model{Name: "llama3", Digest: "sha256:abc"}
	got := ModelFromProto(in.Proto())
	if got != in {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, in)
	}
}

func TestModelsRoundTrip(t *testing.T) {
	t.Parallel()
	in := []Model{{Name: "a"}, {Name: "b", Digest: "d"}}
	got := ModelsFromProto(ModelsToProto(in))
	if len(got) != len(in) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("element %d mismatch: got %+v want %+v", i, got[i], in[i])
		}
	}
	// nil should map to nil, not an empty slice.
	if ModelsToProto(nil) != nil {
		t.Fatal("ModelsToProto(nil) should be nil")
	}
	if ModelsFromProto(nil) != nil {
		t.Fatal("ModelsFromProto(nil) should be nil")
	}
}

// TestChatJobRoundTrip proves the OpenAI chat contract (messages + tools and,
// on the result/chunk, tool_calls + finish_reason + the token split) survives
// the proto round trip additively, so chat semantics genuinely thread
// server<->worker.
func TestChatJobRoundTrip(t *testing.T) {
	t.Parallel()

	in := Job{
		ID:    "j1",
		Model: "llama3",
		Messages: []Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "weather in paris?"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", FunctionName: "get_weather", Arguments: `{"city":"paris"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: `{"temp":21}`},
		},
		Tools: []Tool{
			{Type: "function", Function: ToolFunction{
				Name:        "get_weather",
				Description: "current weather",
				Parameters:  `{"type":"object","properties":{"city":{"type":"string"}}}`,
			}},
		},
	}
	if got := JobFromProto(in.Proto()); !reflect.DeepEqual(got, in) {
		t.Fatalf("chat job round trip mismatch:\n got %+v\nwant %+v", got, in)
	}

	res := JobResult{
		JobID:            "j1",
		FinishReason:     "tool_calls",
		PromptTokens:     11,
		CompletionTokens: 3,
		Tokens:           14,
		ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", FunctionName: "get_weather", Arguments: `{"city":"paris"}`},
		},
	}
	if got := JobResultFromProto(res.Proto()); !reflect.DeepEqual(got, res) {
		t.Fatalf("chat result round trip mismatch:\n got %+v\nwant %+v", got, res)
	}

	chunk := JobChunk{
		JobID:            "j1",
		Done:             true,
		FinishReason:     "tool_calls",
		PromptTokens:     11,
		CompletionTokens: 3,
		Tokens:           14,
		ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", FunctionName: "get_weather", Arguments: `{"city":"paris"}`},
		},
	}
	if got := JobChunkFromProto(chunk.Proto()); !reflect.DeepEqual(got, chunk) {
		t.Fatalf("chat chunk round trip mismatch:\n got %+v\nwant %+v", got, chunk)
	}
}

func TestJobResultRoundTrip(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		in := JobResult{JobID: "j1", Output: "world", Tokens: 7}
		got := JobResultFromProto(in.Proto())
		if got.JobID != in.JobID || got.Output != in.Output || got.Err != nil {
			t.Fatalf("round trip mismatch: got %+v want %+v", got, in)
		}
		if got.Tokens != in.Tokens {
			t.Fatalf("tokens did not survive round trip: got %d want %d", got.Tokens, in.Tokens)
		}
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		in := JobResult{JobID: "j1", Err: &JobError{Code: "boom", Message: "bad"}}
		got := JobResultFromProto(in.Proto())
		if got.Err == nil {
			t.Fatal("expected error to survive round trip")
		}
		if *got.Err != *in.Err {
			t.Fatalf("error mismatch: got %+v want %+v", got.Err, in.Err)
		}
	})
}

func TestNilProtoConversions(t *testing.T) {
	t.Parallel()
	if !reflect.DeepEqual(JobFromProto(nil), Job{}) {
		t.Fatal("JobFromProto(nil) should be zero value")
	}
	if ModelFromProto(nil) != (Model{}) {
		t.Fatal("ModelFromProto(nil) should be zero value")
	}
	if JobErrorFromProto(nil) != nil {
		t.Fatal("JobErrorFromProto(nil) should be nil")
	}
	var je *JobError
	if je.Proto() != nil {
		t.Fatal("(*JobError)(nil).Proto() should be nil")
	}
	if !reflect.DeepEqual(JobResultFromProto(nil), JobResult{}) {
		t.Fatal("JobResultFromProto(nil) should be zero value")
	}
	// Ensure the generated proto type is actually wired up.
	var _ *agentgpuv1.Job = (Job{}).Proto()
}

func TestHeartbeatRoundTrip(t *testing.T) {
	t.Parallel()
	in := Heartbeat{
		WorkerID:        "w1",
		ActiveJobs:      3,
		TotalVRAM:       24 << 30,
		FreeVRAM:        12 << 30,
		Load:            55,
		GPUType:         "test-gpu",
		AvailableModels: []Model{{Name: "llama3", Digest: "abc"}, {Name: "mistral"}},
	}
	got := HeartbeatFromProto(in.Proto())
	if got.WorkerID != in.WorkerID || got.ActiveJobs != in.ActiveJobs ||
		got.TotalVRAM != in.TotalVRAM || got.FreeVRAM != in.FreeVRAM ||
		got.Load != in.Load || got.GPUType != in.GPUType {
		t.Fatalf("scalar fields did not survive round trip: %+v", got)
	}
	if len(got.AvailableModels) != 2 || got.AvailableModels[0] != in.AvailableModels[0] {
		t.Fatalf("available models did not survive round trip: %+v", got.AvailableModels)
	}
	if z := HeartbeatFromProto(nil); z.WorkerID != "" || z.ActiveJobs != 0 || z.AvailableModels != nil {
		t.Fatalf("HeartbeatFromProto(nil) should be zero value, got %+v", z)
	}
}

func TestWorkerStatusString(t *testing.T) {
	t.Parallel()
	cases := map[WorkerStatus]string{
		WorkerOnline:     "online",
		WorkerDraining:   "draining",
		WorkerStale:      "stale",
		WorkerStatus(99): "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Fatalf("WorkerStatus(%d).String() = %q, want %q", s, got, want)
		}
	}
}
