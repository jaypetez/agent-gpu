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

// Message is one entry in an OpenAI-style chat conversation. It is transport-
// neutral: the public API maps OpenAI request messages onto it, the worker
// threads it to Ollama /api/chat, and Ollama tool calls map back onto a
// response Message's ToolCalls.
type Message struct {
	Role       string
	Content    string
	ToolCallID string
	Name       string
	ToolCalls  []ToolCall
}

// Tool is a function the model may call (OpenAI function-calling). Only function
// tools exist today.
type Tool struct {
	Type     string // "function"
	Function ToolFunction
}

// ToolFunction is a JSON-schema function definition. Parameters is the raw
// JSON-schema parameter object encoded as a JSON string so the schema passes
// through unchanged end-to-end.
type ToolFunction struct {
	Name        string
	Description string
	Parameters  string // JSON-encoded JSON-schema object
}

// ToolCall is a function invocation the model emitted, or a prior assistant
// tool call replayed back to the model. Arguments is the raw JSON arguments
// string.
type ToolCall struct {
	ID           string
	Type         string // "function"
	FunctionName string
	Arguments    string // JSON-encoded arguments
}

// Job is a single unit of inference work. Prompt backs /v1/completions and the
// foundational stub; Messages+Tools back /v1/chat/completions. A chat job
// carries Messages (Prompt empty); a completion job carries Prompt (Messages
// empty). The two are additive — existing prompt-only callers are unaffected.
type Job struct {
	ID       string
	Model    string
	Prompt   string
	Messages []Message
	Tools    []Tool
	// SessionID is the owning conversation's session id, used SERVER-SIDE ONLY for
	// session-affinity routing (#34): it lets the dispatcher prefer the worker the
	// session is bound to (warm KV cache). It is a routing hint that never crosses
	// the wire — Proto/JobFromProto deliberately omit it — so the worker contract
	// is unchanged. Empty for jobs with no session (the default, affinity-free).
	SessionID string
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
	// ToolCalls are the function calls the model emitted (OpenAI function-
	// calling); empty for an ordinary text completion.
	ToolCalls []ToolCall
	// FinishReason is the OpenAI finish_reason ("stop", "length", "tool_calls").
	// Empty when the worker reports none.
	FinishReason string
	// PromptTokens/CompletionTokens are the prompt/completion token split for
	// OpenAI usage accounting; Tokens above remains the total used for quota.
	// Zero when unknown.
	PromptTokens     uint64
	CompletionTokens uint64
}

// JobChunk is an incremental piece of a streaming job's output, sent
// worker -> server as tokens are produced. It mirrors the agentgpuv1.JobChunk
// wire message. Per-token chunks carry Delta (with Done false); the terminal
// chunk carries Done true plus the total Tokens, or Done true plus Err on
// failure.
type JobChunk struct {
	JobID  string
	Delta  string
	Done   bool
	Err    *JobError
	Tokens uint64
	// ToolCalls/FinishReason are carried on the terminal chunk so a streaming
	// function call surfaces as delta tool_calls plus finish_reason "tool_calls".
	ToolCalls    []ToolCall
	FinishReason string
	// PromptTokens/CompletionTokens are the terminal-chunk token split for OpenAI
	// usage accounting.
	PromptTokens     uint64
	CompletionTokens uint64
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
	// RegisteredAt is when the worker registered with the server (the server
	// clock at registration). It backs the worker uptime metric (#24), which
	// dashboards derive as time() - start_time. It is the zero time for a snapshot
	// taken before a registration timestamp was recorded; uptime resets on
	// reconnect because a re-registering worker gets a fresh server-side struct.
	RegisteredAt time.Time
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

// ---- chat message / tool conversions ----

// Proto converts a ToolCall to its protobuf representation.
func (t ToolCall) Proto() *agentgpuv1.ToolCall {
	return &agentgpuv1.ToolCall{
		Id:            t.ID,
		Type:          t.Type,
		FunctionName:  t.FunctionName,
		ArgumentsJson: t.Arguments,
	}
}

// ToolCallFromProto converts a protobuf ToolCall to the domain type.
func ToolCallFromProto(p *agentgpuv1.ToolCall) ToolCall {
	if p == nil {
		return ToolCall{}
	}
	return ToolCall{
		ID:           p.GetId(),
		Type:         p.GetType(),
		FunctionName: p.GetFunctionName(),
		Arguments:    p.GetArgumentsJson(),
	}
}

// ToolCallsToProto converts a slice of domain ToolCalls to protobuf.
func ToolCallsToProto(ts []ToolCall) []*agentgpuv1.ToolCall {
	if len(ts) == 0 {
		return nil
	}
	out := make([]*agentgpuv1.ToolCall, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Proto())
	}
	return out
}

// ToolCallsFromProto converts a slice of protobuf ToolCalls to domain.
func ToolCallsFromProto(ps []*agentgpuv1.ToolCall) []ToolCall {
	if len(ps) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(ps))
	for _, p := range ps {
		out = append(out, ToolCallFromProto(p))
	}
	return out
}

// Proto converts a Message to its protobuf representation.
func (m Message) Proto() *agentgpuv1.ChatMessage {
	return &agentgpuv1.ChatMessage{
		Role:       m.Role,
		Content:    m.Content,
		ToolCallId: m.ToolCallID,
		Name:       m.Name,
		ToolCalls:  ToolCallsToProto(m.ToolCalls),
	}
}

// MessageFromProto converts a protobuf ChatMessage to the domain type.
func MessageFromProto(p *agentgpuv1.ChatMessage) Message {
	if p == nil {
		return Message{}
	}
	return Message{
		Role:       p.GetRole(),
		Content:    p.GetContent(),
		ToolCallID: p.GetToolCallId(),
		Name:       p.GetName(),
		ToolCalls:  ToolCallsFromProto(p.GetToolCalls()),
	}
}

func messagesToProto(ms []Message) []*agentgpuv1.ChatMessage {
	if len(ms) == 0 {
		return nil
	}
	out := make([]*agentgpuv1.ChatMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Proto())
	}
	return out
}

func messagesFromProto(ps []*agentgpuv1.ChatMessage) []Message {
	if len(ps) == 0 {
		return nil
	}
	out := make([]Message, 0, len(ps))
	for _, p := range ps {
		out = append(out, MessageFromProto(p))
	}
	return out
}

// Proto converts a Tool to its protobuf representation.
func (t Tool) Proto() *agentgpuv1.Tool {
	return &agentgpuv1.Tool{
		Type: t.Type,
		Function: &agentgpuv1.ToolFunction{
			Name:           t.Function.Name,
			Description:    t.Function.Description,
			ParametersJson: t.Function.Parameters,
		},
	}
}

// ToolFromProto converts a protobuf Tool to the domain type.
func ToolFromProto(p *agentgpuv1.Tool) Tool {
	if p == nil {
		return Tool{}
	}
	fn := p.GetFunction()
	return Tool{
		Type: p.GetType(),
		Function: ToolFunction{
			Name:        fn.GetName(),
			Description: fn.GetDescription(),
			Parameters:  fn.GetParametersJson(),
		},
	}
}

func toolsToProto(ts []Tool) []*agentgpuv1.Tool {
	if len(ts) == 0 {
		return nil
	}
	out := make([]*agentgpuv1.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Proto())
	}
	return out
}

func toolsFromProto(ps []*agentgpuv1.Tool) []Tool {
	if len(ps) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(ps))
	for _, p := range ps {
		out = append(out, ToolFromProto(p))
	}
	return out
}

// Proto converts a Job to its protobuf representation.
func (j Job) Proto() *agentgpuv1.Job {
	return &agentgpuv1.Job{
		Id:       j.ID,
		Model:    j.Model,
		Prompt:   j.Prompt,
		Messages: messagesToProto(j.Messages),
		Tools:    toolsToProto(j.Tools),
	}
}

// JobFromProto converts a protobuf Job to the domain type.
func JobFromProto(p *agentgpuv1.Job) Job {
	if p == nil {
		return Job{}
	}
	return Job{
		ID:       p.GetId(),
		Model:    p.GetModel(),
		Prompt:   p.GetPrompt(),
		Messages: messagesFromProto(p.GetMessages()),
		Tools:    toolsFromProto(p.GetTools()),
	}
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
		JobId:            r.JobID,
		Output:           r.Output,
		Error:            r.Err.Proto(),
		Tokens:           r.Tokens,
		ToolCalls:        ToolCallsToProto(r.ToolCalls),
		FinishReason:     r.FinishReason,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
	}
}

// Proto converts a JobChunk to its protobuf representation.
func (c JobChunk) Proto() *agentgpuv1.JobChunk {
	return &agentgpuv1.JobChunk{
		JobId:            c.JobID,
		Delta:            c.Delta,
		Done:             c.Done,
		Error:            c.Err.Proto(),
		Tokens:           c.Tokens,
		ToolCalls:        ToolCallsToProto(c.ToolCalls),
		FinishReason:     c.FinishReason,
		PromptTokens:     c.PromptTokens,
		CompletionTokens: c.CompletionTokens,
	}
}

// JobChunkFromProto converts a protobuf JobChunk to the domain type.
func JobChunkFromProto(p *agentgpuv1.JobChunk) JobChunk {
	if p == nil {
		return JobChunk{}
	}
	return JobChunk{
		JobID:            p.GetJobId(),
		Delta:            p.GetDelta(),
		Done:             p.GetDone(),
		Err:              JobErrorFromProto(p.GetError()),
		Tokens:           p.GetTokens(),
		ToolCalls:        ToolCallsFromProto(p.GetToolCalls()),
		FinishReason:     p.GetFinishReason(),
		PromptTokens:     p.GetPromptTokens(),
		CompletionTokens: p.GetCompletionTokens(),
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
		JobID:            p.GetJobId(),
		Output:           p.GetOutput(),
		Err:              JobErrorFromProto(p.GetError()),
		Tokens:           p.GetTokens(),
		ToolCalls:        ToolCallsFromProto(p.GetToolCalls()),
		FinishReason:     p.GetFinishReason(),
		PromptTokens:     p.GetPromptTokens(),
		CompletionTokens: p.GetCompletionTokens(),
	}
}
