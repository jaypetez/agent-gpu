package types

import (
	"errors"
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
	if got != in {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, in)
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
	if JobFromProto(nil) != (Job{}) {
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
	if JobResultFromProto(nil) != (JobResult{}) {
		t.Fatal("JobResultFromProto(nil) should be zero value")
	}
	// Ensure the generated proto type is actually wired up.
	var _ *agentgpuv1.Job = (Job{}).Proto()
}
