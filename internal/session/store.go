package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// enforces the per-session turn-count AND cumulative-byte caps on Append:
//
// Cap policy (TRIM-OLDEST): when appending a turn would exceed either cap, the
// oldest turns are dropped until both caps hold for the resulting slice (with
// the new turn included). This keeps a conversation usable — the most recent
// context is always retained — rather than rejecting writes and stalling a live
// chat. A single turn larger than the byte cap is still stored (it becomes the
// sole turn): the alternative, refusing it, would make the session permanently
// unwritable.
//
// Implementations MUST be safe for concurrent use and MUST return deep copies
// from Get so a caller cannot mutate stored turns.
type HistoryStore interface {
	// Append adds turn to sessionID's history, enforcing the configured caps by
	// trimming oldest turns as documented above.
	Append(sessionID string, turn types.Message) error
	// Get returns a copy of sessionID's ordered turns (oldest first). A session
	// with no history returns an empty slice and no error.
	Get(sessionID string) ([]types.Message, error)
	// DeleteBySession removes all history for sessionID. Deleting missing history
	// is not an error.
	DeleteBySession(sessionID string) error
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
// per-session turn-count and cumulative-byte caps with the trim-oldest policy
// documented on HistoryStore. History survives restarts via Checkpoint/
// LoadCheckpoint.
type MemoryHistoryStore struct {
	maxTurns int
	maxBytes int

	mu      sync.RWMutex
	history map[string][]types.Message
}

// NewMemoryHistoryStore returns an empty in-memory HistoryStore bounded by
// maxTurns (turn count) and maxBytes (cumulative content bytes). A non-positive
// cap means "unbounded" for that dimension.
func NewMemoryHistoryStore(maxTurns, maxBytes int) *MemoryHistoryStore {
	return &MemoryHistoryStore{
		maxTurns: maxTurns,
		maxBytes: maxBytes,
		history:  make(map[string][]types.Message),
	}
}

// Append implements HistoryStore, trimming oldest turns to keep both caps.
func (m *MemoryHistoryStore) Append(sessionID string, turn types.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	turns := append(m.history[sessionID], cloneMessage(turn))
	m.history[sessionID] = m.trim(turns)
	return nil
}

// trim drops oldest turns until both the turn-count and byte caps hold. The most
// recent turn is always retained even if it alone exceeds the byte cap (the
// alternative — rejecting it — would wedge the session). Callers hold m.mu.
func (m *MemoryHistoryStore) trim(turns []types.Message) []types.Message {
	if m.maxTurns > 0 && len(turns) > m.maxTurns {
		// Drop from the front; re-slice onto a fresh backing array so the trimmed
		// turns become collectable and we do not retain the old (larger) array.
		turns = append([]types.Message(nil), turns[len(turns)-m.maxTurns:]...)
	}
	if m.maxBytes > 0 {
		total := 0
		for _, t := range turns {
			total += turnBytes(t)
		}
		for total > m.maxBytes && len(turns) > 1 {
			total -= turnBytes(turns[0])
			turns = turns[1:]
		}
		// Compact onto a fresh array if we dropped any leading turns above so the
		// dropped content is not retained by the underlying array.
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
