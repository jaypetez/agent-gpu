package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// Permission bits for checkpoint files and their parent directory, matching the
// rest of the control-plane state (owner-only). Sessions and history hold no
// secrets, but defense in depth is cheap and keeps the on-disk surface uniform.
const (
	checkpointMode    os.FileMode = 0o600
	checkpointDirMode os.FileMode = 0o700
)

// SessionStore is the persistence seam for sessions. Implementations MUST be
// safe for concurrent use and MUST hand callers deep copies (so a returned
// Session can be mutated without corrupting stored state). It is intentionally
// minimal — Put overwrites, Delete-of-missing is not an error — mirroring
// store.Store so a Redis/Postgres backend can slot in later without touching the
// Manager.
type SessionStore interface {
	// Put stores (or overwrites) a session by its ID.
	Put(s Session) error
	// Get returns a copy of the session with id, or ErrSessionNotFound.
	Get(id string) (Session, error)
	// List returns copies of all stored sessions.
	List() ([]Session, error)
	// Delete removes a session. Deleting a missing session is not an error.
	Delete(id string) error
}

// HistoryStore is the persistence seam for per-session conversation history. It
// enforces the per-session turn-count, cumulative-byte, AND cumulative
// context-token caps on Append under a configurable overflow policy (#37):
//
// OverflowTrim (DEFAULT): when appending a turn would exceed any cap, the oldest
// turns are dropped until every cap holds for the resulting slice (with the new
// turn included). This keeps a conversation usable — the most recent context is
// always retained — rather than rejecting writes and stalling a live chat. A
// single turn larger than the byte/token cap is still stored (it becomes the
// sole turn): the alternative, refusing it, would make the session permanently
// unwritable.
//
// OverflowReject: an append that would exceed any cap returns
// ErrSessionLimitExceeded and leaves the stored history unchanged, enforcing a
// hard per-session ceiling. A turn that still fits is appended normally; a
// first turn that alone exceeds a cap is rejected (there is no prior context to
// preserve, so the hard ceiling applies).
//
// Implementations MUST be safe for concurrent use and MUST return deep copies
// from Get so a caller cannot mutate stored turns.
type HistoryStore interface {
	// Append adds turn to sessionID's history, enforcing the configured caps under
	// the configured overflow policy (trim oldest, or reject with
	// ErrSessionLimitExceeded) as documented above.
	Append(sessionID string, turn types.Message) error
	// AppendBatch adds several turns ATOMICALLY under OverflowReject: if the whole
	// batch would exceed a cap it returns ErrSessionLimitExceeded and stores none
	// of them (so a multi-message turn — user message(s) plus the assistant reply —
	// never persists a half-turn). Under OverflowTrim it appends the turns in order
	// with trimming, equivalent to calling Append for each. An empty batch is a
	// no-op.
	AppendBatch(sessionID string, turns ...types.Message) error
	// Get returns a copy of sessionID's ordered turns (oldest first). A session
	// with no history returns an empty slice and no error.
	Get(sessionID string) ([]types.Message, error)
	// DeleteBySession removes all history for sessionID. Deleting missing history
	// is not an error.
	DeleteBySession(sessionID string) error
	// WouldReject reports whether appending the given turns to sessionID's current
	// history would be REJECTED under the store's overflow policy (#37). It is
	// always false under OverflowTrim (trimming never rejects). Under
	// OverflowReject it returns true iff the resulting history would exceed a cap.
	// It is the read-only pre-dispatch check the HTTP stateful-chat path uses to
	// reject a turn BEFORE running inference, so no work is wasted on a turn whose
	// persistence would be refused. It does not mutate stored history.
	WouldReject(sessionID string, turns ...types.Message) bool
}

// cloneMessage deep-copies a chat turn so the store and callers never share the
// ToolCalls backing array.
func cloneMessage(m types.Message) types.Message {
	out := m
	if m.ToolCalls != nil {
		out.ToolCalls = append([]types.ToolCall(nil), m.ToolCalls...)
	}
	return out
}

// cloneMessages deep-copies a slice of turns.
func cloneMessages(ms []types.Message) []types.Message {
	if ms == nil {
		return nil
	}
	out := make([]types.Message, len(ms))
	for i, m := range ms {
		out[i] = cloneMessage(m)
	}
	return out
}

// turnBytes is the cumulative-byte cost of a turn. It counts the user-supplied
// text fields (the only unbounded inputs) so the byte cap bounds memory growth
// from conversation content; fixed-size metadata is ignored.
func turnBytes(m types.Message) int {
	n := len(m.Role) + len(m.Content) + len(m.ToolCallID) + len(m.Name)
	for _, tc := range m.ToolCalls {
		n += len(tc.ID) + len(tc.Type) + len(tc.FunctionName) + len(tc.Arguments)
	}
	return n
}

// turnTokens is the estimated context-token cost of a turn. There is no real
// tokenizer in the project, so this uses the same whitespace-token heuristic the
// echo executor counts output tokens with (len(strings.Fields(...))) applied to
// the user-supplied text fields plus any tool-call function name/arguments. It
// is a documented, deterministic estimate — NOT an exact model token count — for
// bounding cumulative conversation size; real per-model counts are out of scope
// (the same simplification quota accounting makes for token budgets).
func turnTokens(m types.Message) int {
	n := len(strings.Fields(m.Content)) + len(strings.Fields(m.Name))
	for _, tc := range m.ToolCalls {
		n += len(strings.Fields(tc.FunctionName)) + len(strings.Fields(tc.Arguments))
	}
	return n
}

// MemorySessionStore is an in-memory, concurrency-safe SessionStore. It is the
// default backend for standalone/dev use and the substrate for tests. Sessions
// survive process restarts via Checkpoint/LoadCheckpoint to a JSON file.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

// NewMemorySessionStore returns an empty in-memory SessionStore.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{sessions: make(map[string]Session)}
}

// Put implements SessionStore.
func (m *MemorySessionStore) Put(s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s.clone()
	return nil
}

// Get implements SessionStore.
func (m *MemorySessionStore) Get(id string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return s.clone(), nil
}

// List implements SessionStore.
func (m *MemorySessionStore) List() ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.clone())
	}
	return out, nil
}

// Delete implements SessionStore. Deleting a missing session is not an error.
func (m *MemorySessionStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

// Checkpoint atomically writes the current sessions to path (write temp +
// rename), creating the parent directory if needed. The durability window is
// the checkpoint cadence: per-operation writes never touch disk (mirroring the
// quota counter store), so up to one interval of session/history mutations can
// be lost on an unclean crash. The cmd layer (#36) checkpoints periodically and
// on graceful shutdown. now is accepted for symmetry with the history store and
// to support a future "roll-expire on checkpoint"; today the snapshot is written
// as-is and expiry is applied on load.
func (m *MemorySessionStore) Checkpoint(path string, now time.Time) error {
	m.mu.RLock()
	snapshot := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		snapshot = append(snapshot, s)
	}
	m.mu.RUnlock()
	return writeCheckpoint(path, ".sessions-*.tmp", snapshot)
}

// LoadCheckpoint replaces the store's sessions with those persisted at path,
// dropping any session already expired as of now (roll-expire on load: a process
// that was down past a session's idle TTL must not resurrect it). A missing file
// is not an error (a fresh store). Returns the ids of the sessions that were
// dropped so the caller can purge their orphaned history.
func (m *MemorySessionStore) LoadCheckpoint(path string, now time.Time) (dropped []string, err error) {
	var loaded []Session
	ok, err := readCheckpoint(path, &loaded)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = make(map[string]Session)
	if !ok {
		return nil, nil
	}
	for _, s := range loaded {
		if s.expired(now) {
			dropped = append(dropped, s.ID)
			continue
		}
		m.sessions[s.ID] = s
	}
	return dropped, nil
}

// MemoryHistoryStore is an in-memory, concurrency-safe HistoryStore enforcing
// per-session turn-count, cumulative-byte, and cumulative context-token caps
// under a configurable overflow policy (trim-oldest by default), as documented
// on HistoryStore. History survives restarts via Checkpoint/LoadCheckpoint.
//
// The caps and overflow policy are runtime-tunable (#92): SetCaps replaces them
// live with no restart. They are guarded by mu — the same lock that serializes
// Append/trim/exceedsCap — so a cap read on the append hot path always sees a
// consistent set and a live update never races a concurrent append. Tightening a
// cap takes effect on the NEXT append (already-stored history is not retroactively
// trimmed until it next grows), matching how LoadCheckpoint re-trims on load.
type MemoryHistoryStore struct {
	mu        sync.RWMutex
	maxTurns  int
	maxBytes  int
	maxTokens int
	policy    OverflowPolicy
	history   map[string][]types.Message
}

// NewMemoryHistoryStore returns an empty in-memory HistoryStore bounded by
// maxTurns (turn count) and maxBytes (cumulative content bytes), with no
// context-token cap and the trim-oldest overflow policy. A non-positive cap
// means "unbounded" for that dimension. It is retained as the original
// two-dimension constructor (so existing callers/tests are unchanged);
// NewMemoryHistoryStoreWithPolicy adds the context-token cap and overflow
// policy (#37).
func NewMemoryHistoryStore(maxTurns, maxBytes int) *MemoryHistoryStore {
	return NewMemoryHistoryStoreWithPolicy(maxTurns, maxBytes, 0, OverflowTrim)
}

// NewMemoryHistoryStoreWithPolicy returns an empty in-memory HistoryStore
// bounded by maxTurns (turn count), maxBytes (cumulative content bytes), and
// maxTokens (cumulative estimated context tokens — see turnTokens), enforced
// under policy. A non-positive cap means "unbounded" for that dimension (#37).
func NewMemoryHistoryStoreWithPolicy(maxTurns, maxBytes, maxTokens int, policy OverflowPolicy) *MemoryHistoryStore {
	return &MemoryHistoryStore{
		maxTurns:  maxTurns,
		maxBytes:  maxBytes,
		maxTokens: maxTokens,
		policy:    policy,
		history:   make(map[string][]types.Message),
	}
}

// HistoryCaps is the runtime-tunable set of per-session history caps and the
// overflow policy (#92). A non-positive cap means "unbounded" for that dimension,
// matching the constructor convention. It is the value SetCaps applies and Caps
// returns, so the admin config layer can read and replace the whole set atomically.
type HistoryCaps struct {
	MaxTurns  int
	MaxBytes  int
	MaxTokens int
	Policy    OverflowPolicy
}

// SetCaps replaces the per-session history caps and overflow policy at runtime
// (#92), under the same lock that serializes appends so the change is atomic with
// respect to a concurrent Append/AppendBatch. It takes effect on the next append
// (stored history is not retroactively trimmed). Safe for concurrent use.
func (m *MemoryHistoryStore) SetCaps(c HistoryCaps) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxTurns = c.MaxTurns
	m.maxBytes = c.MaxBytes
	m.maxTokens = c.MaxTokens
	m.policy = c.Policy
}

// Caps returns the current history caps and overflow policy (#92), read under mu
// so it reflects any live SetCaps. It backs the admin config GET projection.
func (m *MemoryHistoryStore) Caps() HistoryCaps {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return HistoryCaps{
		MaxTurns:  m.maxTurns,
		MaxBytes:  m.maxBytes,
		MaxTokens: m.maxTokens,
		Policy:    m.policy,
	}
}

// Append implements HistoryStore. Under OverflowTrim it trims oldest turns to
// keep every cap; under OverflowReject it returns ErrSessionLimitExceeded and
// leaves the stored history untouched when the new turn would exceed a cap.
func (m *MemoryHistoryStore) Append(sessionID string, turn types.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.history[sessionID]
	turns := append(existing, cloneMessage(turn))
	if m.policy == OverflowReject && m.exceedsCap(turns) {
		// Reject without mutating stored history: append produced a fresh header but
		// the backing array up to len(existing) may be shared, so we simply do not
		// store the new slice. The existing turns remain as they were.
		return ErrSessionLimitExceeded
	}
	m.history[sessionID] = m.trim(turns)
	return nil
}

// AppendBatch implements HistoryStore: atomic, all-or-nothing append under
// OverflowReject so a multi-message turn never persists a half-turn.
func (m *MemoryHistoryStore) AppendBatch(sessionID string, turns ...types.Message) error {
	if len(turns) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	combined := append(m.history[sessionID], cloneMessages(turns)...)
	if m.policy == OverflowReject && m.exceedsCap(combined) {
		return ErrSessionLimitExceeded
	}
	m.history[sessionID] = m.trim(combined)
	return nil
}

// WouldReject implements HistoryStore: it reports whether appending turns to
// sessionID's current history would be refused under OverflowReject. It is
// always false under OverflowTrim. It takes the read lock and does not mutate.
func (m *MemoryHistoryStore) WouldReject(sessionID string, turns ...types.Message) bool {
	if m.policy != OverflowReject || len(turns) == 0 {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	combined := make([]types.Message, 0, len(m.history[sessionID])+len(turns))
	combined = append(combined, m.history[sessionID]...)
	combined = append(combined, turns...)
	return m.exceedsCap(combined)
}

// exceedsCap reports whether turns violates any configured cap (turns, bytes, or
// context tokens). It is the OverflowReject predicate; a single turn larger than
// the byte/token cap is treated as exceeding so the hard ceiling is enforced.
// Callers hold m.mu (read or write).
func (m *MemoryHistoryStore) exceedsCap(turns []types.Message) bool {
	if m.maxTurns > 0 && len(turns) > m.maxTurns {
		return true
	}
	if m.maxBytes > 0 {
		total := 0
		for _, t := range turns {
			total += turnBytes(t)
		}
		if total > m.maxBytes {
			return true
		}
	}
	if m.maxTokens > 0 {
		total := 0
		for _, t := range turns {
			total += turnTokens(t)
		}
		if total > m.maxTokens {
			return true
		}
	}
	return false
}

// trim drops oldest turns until the turn-count, byte, and context-token caps all
// hold. The most recent turn is always retained even if it alone exceeds the
// byte/token cap (the alternative — rejecting it — would wedge the session under
// the trim policy). Callers hold m.mu.
func (m *MemoryHistoryStore) trim(turns []types.Message) []types.Message {
	dropped := false
	if m.maxTurns > 0 && len(turns) > m.maxTurns {
		turns = turns[len(turns)-m.maxTurns:]
		dropped = true
	}
	if m.maxBytes > 0 {
		total := 0
		for _, t := range turns {
			total += turnBytes(t)
		}
		for total > m.maxBytes && len(turns) > 1 {
			total -= turnBytes(turns[0])
			turns = turns[1:]
			dropped = true
		}
	}
	if m.maxTokens > 0 {
		total := 0
		for _, t := range turns {
			total += turnTokens(t)
		}
		for total > m.maxTokens && len(turns) > 1 {
			total -= turnTokens(turns[0])
			turns = turns[1:]
			dropped = true
		}
	}
	if dropped {
		// Compact onto a fresh array so the dropped leading turns become collectable
		// and we do not retain the old (larger) backing array.
		turns = append([]types.Message(nil), turns...)
	}
	return turns
}

// Get implements HistoryStore.
func (m *MemoryHistoryStore) Get(sessionID string) ([]types.Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	turns := m.history[sessionID]
	out := cloneMessages(turns)
	if out == nil {
		out = []types.Message{}
	}
	return out, nil
}

// DeleteBySession implements HistoryStore. Deleting missing history is fine.
func (m *MemoryHistoryStore) DeleteBySession(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.history, sessionID)
	return nil
}

// Checkpoint atomically writes all session histories to path (write temp +
// rename). See MemorySessionStore.Checkpoint for the durability-window note.
func (m *MemoryHistoryStore) Checkpoint(path string) error {
	m.mu.RLock()
	snapshot := make(map[string][]types.Message, len(m.history))
	for id, turns := range m.history {
		snapshot[id] = cloneMessages(turns)
	}
	m.mu.RUnlock()
	return writeCheckpoint(path, ".history-*.tmp", snapshot)
}

// LoadCheckpoint replaces the store's history with that persisted at path,
// keeping only the histories whose session id is in keep (so history orphaned by
// an expired/dropped session is not resurrected). A nil keep map keeps all
// loaded history. A missing file is not an error. Loaded turns are re-trimmed to
// the current caps so a config change that tightened a cap is honored on load.
func (m *MemoryHistoryStore) LoadCheckpoint(path string, keep map[string]struct{}) error {
	var loaded map[string][]types.Message
	ok, err := readCheckpoint(path, &loaded)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = make(map[string][]types.Message)
	if !ok {
		return nil
	}
	for id, turns := range loaded {
		if keep != nil {
			if _, want := keep[id]; !want {
				continue
			}
		}
		m.history[id] = m.trim(turns)
	}
	return nil
}

// writeCheckpoint marshals v to JSON and writes it to path atomically (temp +
// rename) with owner-only permissions, creating the parent directory if needed.
// tmpPattern is the os.CreateTemp pattern for the staging file.
func writeCheckpoint(path, tmpPattern string, v any) error {
	if path == "" {
		return fmt.Errorf("session: empty checkpoint path")
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal checkpoint: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, checkpointDirMode); err != nil {
		return fmt.Errorf("session: create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("session: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(checkpointMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("session: rename checkpoint: %w", err)
	}
	return nil
}

// readCheckpoint reads and JSON-decodes path into v. It reports whether the file
// had decodable content: a missing or empty file yields (false, nil) so the
// caller treats it as a fresh store.
func readCheckpoint(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("session: read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("session: parse checkpoint %s: %w", path, err)
	}
	return true, nil
}

// Compile-time assertions that the in-memory stores satisfy their interfaces.
var (
	_ SessionStore = (*MemorySessionStore)(nil)
	_ HistoryStore = (*MemoryHistoryStore)(nil)
)
