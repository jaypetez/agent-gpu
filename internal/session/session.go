// Package session implements agent-gpu's persisted conversation session
// abstraction and its bounded conversation-history store.
//
// A Session is a long-lived, owner-scoped handle to a model conversation: it
// records who owns it (the public API-key id, never a secret), which model it
// targets, an optional bound worker (affinity routing lands in #34), and its
// idle lifecycle (created/last-active timestamps + a per-session idle TTL). A
// companion HistoryStore keeps the ordered chat turns for each session, bounded
// by configurable turn-count and cumulative-byte caps.
//
// This package is the FOUNDATION of the sessions epic (#34 affinity routing,
// #35 keep_alive, #36 the HTTP session API). It deliberately ships only the
// domain model, the pluggable persistence interfaces, in-memory + checkpointing
// implementations, a Manager with owner-checked lifecycle operations, and an
// idle-expiry sweeper. Wiring into the cmd/HTTP layers is OUT OF SCOPE and is
// left as a documented seam for #36: construct a Manager (NewManager), call
// Start to run the sweeper, and Close on shutdown.
package session

import (
	"errors"
	"time"
)

// ErrSessionNotFound is returned when a session does not exist OR exists but is
// not owned by the requesting key. The two cases are deliberately indistinct so
// the API never leaks the existence of another owner's session. Match it with
// errors.Is.
var ErrSessionNotFound = errors.New("session: not found")

// Status is the lifecycle state of a session.
type Status string

const (
	// StatusActive is a live session eligible for resume and new turns.
	StatusActive Status = "active"
	// StatusExpired marks a session whose idle TTL elapsed. The sweeper deletes
	// expired sessions (and their history); the status exists so a session loaded
	// from a checkpoint can be classified before deletion and so callers reading a
	// just-expired session see a definite state rather than a silent absence.
	StatusExpired Status = "expired"
)

// Session is a persisted, owner-scoped conversation handle.
//
// It stores ONLY public identifiers: OwnerKeyID is the API key's public id (the
// "keyid"), never the secret or its hash. BoundWorkerID is a plain stored field
// today — affinity routing that honors it is #34.
type Session struct {
	// ID is the opaque, unguessable session id ("sess_" + crypto/rand hex).
	ID string
	// OwnerKeyID is the public API-key id that owns this session. NEVER a secret.
	OwnerKeyID string
	// Model is the inference model the session targets.
	Model string
	// BoundWorkerID is the worker this session prefers for affinity (#34). It is a
	// stored field only here; nothing in this package routes on it.
	BoundWorkerID string
	// CreatedAt is when the session was created.
	CreatedAt time.Time
	// LastActiveAt is the timestamp of the most recent activity (create, resume,
	// or appended turn). The sweeper expires a session when now-LastActiveAt > TTL.
	LastActiveAt time.Time
	// TTL is the per-session idle timeout. A non-positive TTL means the session
	// never idles out (the sweeper skips it).
	TTL time.Duration
	// Status is the lifecycle state (active/expired).
	Status Status
}

// expired reports whether the session has idled out as of now. A non-positive
// TTL means "never expires".
func (s Session) expired(now time.Time) bool {
	if s.TTL <= 0 {
		return false
	}
	return now.Sub(s.LastActiveAt) > s.TTL
}

// clone returns a deep copy of the session so the store and its callers never
// share mutable state. Session has no reference-typed fields today, so a value
// copy suffices; the helper is kept explicit so a future slice/pointer field is
// copied here rather than silently aliased.
func (s Session) clone() Session { return s }
