package testutil

import "github.com/jaypetez/agent-gpu/internal/types"

// Default job field values, used when no option overrides them. They are chosen
// so a zero-argument Job() is valid per types.Job.Validate (non-empty ID + Model)
// and dispatchable through the echo path.
const (
	// DefaultModel is the model a built Job/Worker/Heartbeat advertises or targets.
	DefaultModel = "llama3"
	// DefaultPrompt is the prompt a built completion Job carries.
	DefaultPrompt = "hi"
	// DefaultJobID is the id a built Job carries unless WithJobID overrides it.
	DefaultJobID = "job-1"
)

// JobOption mutates a types.Job during construction. Options are applied in the
// order given, so a later option overrides an earlier one.
type JobOption func(*types.Job)

// Job builds a types.Job with sane defaults (DefaultJobID, DefaultModel,
// DefaultPrompt), then applies opts in order. The zero-argument form returns a
// valid completion job; pass WithMessages to make it a chat job (clear the prompt
// with WithPrompt("") if a pure chat job is required).
func Job(opts ...JobOption) types.Job {
	j := types.Job{
		ID:     DefaultJobID,
		Model:  DefaultModel,
		Prompt: DefaultPrompt,
	}
	for _, opt := range opts {
		opt(&j)
	}
	return j
}

// WithJobID sets the job id.
func WithJobID(id string) JobOption {
	return func(j *types.Job) { j.ID = id }
}

// WithModel sets the job's target model.
func WithModel(model string) JobOption {
	return func(j *types.Job) { j.Model = model }
}

// WithPrompt sets the completion prompt. Pass "" to clear it for a chat-only job.
func WithPrompt(prompt string) JobOption {
	return func(j *types.Job) { j.Prompt = prompt }
}

// WithMessages sets the chat conversation. Building a chat job typically pairs
// this with WithPrompt("") so only Messages drive the request.
func WithMessages(msgs ...types.Message) JobOption {
	return func(j *types.Job) { j.Messages = msgs }
}

// WithTools sets the function-calling tools advertised to the model.
func WithTools(tools ...types.Tool) JobOption {
	return func(j *types.Job) { j.Tools = tools }
}

// WithSessionID sets the server-side session-affinity routing hint. It never
// crosses the wire (see types.Job.SessionID); empty means no session.
func WithSessionID(id string) JobOption {
	return func(j *types.Job) { j.SessionID = id }
}

// UserMessage is a convenience constructor for a user-role chat message.
func UserMessage(content string) types.Message {
	return types.Message{Role: "user", Content: content}
}

// AssistantMessage is a convenience constructor for an assistant-role chat message.
func AssistantMessage(content string) types.Message {
	return types.Message{Role: "assistant", Content: content}
}
