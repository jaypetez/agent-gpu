// Package types holds the core shared domain types for agent-gpu.
//
// These are deliberately small, transport-neutral Go types. The gRPC wire
// contract lives in proto/agentgpu/v1; this package provides ergonomic Go
// representations plus conversions to/from the generated protobuf messages so
// the rest of the codebase does not pass *.pb.go types around directly.
package types

import (
	"errors"
	"time"

	agentgpuv1 "github.com/jaypetez/agent-gpu/proto/agentgpu/v1"
)

// Model describes an inference model a worker can serve.
type Model struct {
	Name   string
	Digest string
}

// Job is a single unit of inference work.
type Job struct {
	ID     string
	Model  string
	Prompt string
}

// JobError is a structured, transport-neutral job failure. It implements the
// error interface so it can flow through ordinary Go error handling.
type JobError struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *JobError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// JobResult is the outcome of executing a Job.
type JobResult struct {
	JobID  string
	Output string
	Err    *JobError
	// Tokens is the number of tokens generated/consumed by the job, reported by
	// the worker for quota accounting (#5). The echo executor reports a
	// whitespace token count; real counts arrive with Ollama (#11). Zero means
	// "no tokens reported".
	Tokens uint64
}

// Heartbeat is a worker's periodic liveness-and-capacity report. It mirrors the
// agentgpuv1.Heartbeat wire message as an ergonomic, transport-neutral type.
type Heartbeat struct {
	WorkerID        string
	ActiveJobs      uint32
	TotalVRAM       uint64
	FreeVRAM        uint64
	Load            uint32
	GPUType         string
	AvailableModels []Model
}

// WorkerStatus is the lifecycle state of a worker in the server's fleet view.
type WorkerStatus int

const (
	// WorkerOnline is a healthy worker eligible to receive new jobs.
	WorkerOnline WorkerStatus = iota
	// WorkerDraining is a worker that requested graceful shutdown: it receives
	// no new jobs but its in-flight jobs are allowed to finish.
	WorkerDraining
	// WorkerStale is a worker that has missed heartbeats past the timeout and is
	// about to be (or has been) evicted.
	WorkerStale
)

// String renders a WorkerStatus for logs and fleet snapshots.
func (s WorkerStatus) String() string {
	switch s {
	case WorkerOnline:
		return "online"
	case WorkerDraining:
		return "draining"
	case WorkerStale:
		return "stale"
	default:
		return "unknown"
	}
}

// Worker is a read-only snapshot of one worker in the server's fleet view. It
// is assembled on demand for observability and (later) scheduling; it is not a
// live handle to the worker's stream.
type Worker struct {
	ID         string
	Models     []Model
	LastSeen   time.Time
	ActiveJobs uint32
	TotalVRAM  uint64
	FreeVRAM   uint64
	Load       uint32
	GPUType    string
	Status     WorkerStatus
}

// ErrInvalidJob is returned when a Job fails validation.
var ErrInvalidJob = errors.New("invalid job")

// Validate reports whether the Job is well-formed enough to dispatch.
func (j Job) Validate() error {
	if j.ID == "" {
		return errors.Join(ErrInvalidJob, errors.New("missing id"))
	}
	if j.Model == "" {
		return errors.Join(ErrInvalidJob, errors.New("missing model"))
	}
	return nil
}

// ---- conversions to/from the generated protobuf types ----

// Proto converts a Model to its protobuf representation.
func (m Model) Proto() *agentgpuv1.Model {
	return &agentgpuv1.Model{Name: m.Name, Digest: m.Digest}
}

// ModelFromProto converts a protobuf Model to the domain type.
func ModelFromProto(p *agentgpuv1.Model) Model {
	if p == nil {
		return Model{}
	}
	return Model{Name: p.GetName(), Digest: p.GetDigest()}
}

// ModelsFromProto converts a slice of protobuf Models to domain Models.
func ModelsFromProto(ps []*agentgpuv1.Model) []Model {
	if ps == nil {
		return nil
	}
	out := make([]Model, 0, len(ps))
	for _, p := range ps {
		out = append(out, ModelFromProto(p))
	}
	return out
}

// ModelsToProto converts a slice of domain Models to protobuf Models.
func ModelsToProto(ms []Model) []*agentgpuv1.Model {
	if ms == nil {
		return nil
	}
	out := make([]*agentgpuv1.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Proto())
	}
	return out
}

// Proto converts a Job to its protobuf representation.
func (j Job) Proto() *agentgpuv1.Job {
	return &agentgpuv1.Job{Id: j.ID, Model: j.Model, Prompt: j.Prompt}
}

// JobFromProto converts a protobuf Job to the domain type.
func JobFromProto(p *agentgpuv1.Job) Job {
	if p == nil {
		return Job{}
	}
	return Job{ID: p.GetId(), Model: p.GetModel(), Prompt: p.GetPrompt()}
}

// Proto converts a JobError to its protobuf representation. nil maps to nil.
func (e *JobError) Proto() *agentgpuv1.Error {
	if e == nil {
		return nil
	}
	return &agentgpuv1.Error{Code: e.Code, Message: e.Message}
}

// JobErrorFromProto converts a protobuf Error to the domain type. nil -> nil.
func JobErrorFromProto(p *agentgpuv1.Error) *JobError {
	if p == nil {
		return nil
	}
	return &JobError{Code: p.GetCode(), Message: p.GetMessage()}
}

// Proto converts a JobResult to its protobuf representation.
func (r JobResult) Proto() *agentgpuv1.JobResult {
	return &agentgpuv1.JobResult{
		JobId:  r.JobID,
		Output: r.Output,
		Error:  r.Err.Proto(),
		Tokens: r.Tokens,
	}
}

// Proto converts a Heartbeat to its protobuf representation.
func (h Heartbeat) Proto() *agentgpuv1.Heartbeat {
	return &agentgpuv1.Heartbeat{
		WorkerId:        h.WorkerID,
		ActiveJobs:      h.ActiveJobs,
		TotalVramBytes:  h.TotalVRAM,
		FreeVramBytes:   h.FreeVRAM,
		Load:            h.Load,
		GpuType:         h.GPUType,
		AvailableModels: ModelsToProto(h.AvailableModels),
	}
}

// HeartbeatFromProto converts a protobuf Heartbeat to the domain type.
func HeartbeatFromProto(p *agentgpuv1.Heartbeat) Heartbeat {
	if p == nil {
		return Heartbeat{}
	}
	return Heartbeat{
		WorkerID:        p.GetWorkerId(),
		ActiveJobs:      p.GetActiveJobs(),
		TotalVRAM:       p.GetTotalVramBytes(),
		FreeVRAM:        p.GetFreeVramBytes(),
		Load:            p.GetLoad(),
		GPUType:         p.GetGpuType(),
		AvailableModels: ModelsFromProto(p.GetAvailableModels()),
	}
}

// JobResultFromProto converts a protobuf JobResult to the domain type.
func JobResultFromProto(p *agentgpuv1.JobResult) JobResult {
	if p == nil {
		return JobResult{}
	}
	return JobResult{
		JobID:  p.GetJobId(),
		Output: p.GetOutput(),
		Err:    JobErrorFromProto(p.GetError()),
		Tokens: p.GetTokens(),
	}
}
