package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

// idPrefix is the session-id namespace, mirroring the OpenAI-style prefixed ids
// used elsewhere (httpapi.newID, auth keyids).
const idPrefix = "sess_"

// Manager owns the session lifecycle over a SessionStore and a HistoryStore. All
// read/write operations are owner-scoped by the caller's API-key id: an
// operation on a session not owned by the supplied key returns ErrSessionNotFound
// (indistinguishable from "no such session", so existence never leaks across
// owners). The Manager also runs the idle-expiry sweeper (Start/Close).
//
// It is the single entry point #36 wires the HTTP session API onto; nothing in
// this package touches cmd or HTTP today.
type Manager struct {
	log      *slog.Logger
	sessions SessionStore
	history  HistoryStore

	// now is the injectable clock (defaults to time.Now); tests fast-forward it
	// instead of sleeping. ttl is the default idle TTL stamped onto new sessions.
	now func() time.Time
	ttl time.Duration

	// randRead is the entropy source for session ids (defaults to crypto/rand);
	// injectable so a test can force an id-generation failure deterministically.
	randRead func([]byte) (int, error)

	// sweepInterval is the wall-clock cadence at which the sweeper wakes to
	// re-evaluate expiry against the (possibly injected) clock.
	sweepInterval time.Duration

	// Sweeper lifecycle, mirroring the server eviction loop: a sync.Once-guarded
	// goroutine, a stop channel closed by Close, and a done channel waited on.
	startOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger sets the structured logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.log = l
		}
	}
}

// WithClock overrides the time source used for lifecycle stamping and idle
// expiry (for tests). A nil clock is ignored. Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithTTL sets the default idle TTL stamped onto sessions created via Create. A
// non-positive value is ignored (the package default applies). A session with a
// non-positive TTL never idles out.
func WithTTL(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.ttl = d
		}
	}
}

// WithSweepInterval overrides the wall-clock cadence at which the sweeper wakes.
// A non-positive value is ignored. Defaults to a fraction of the TTL. Primarily
// a test seam so the sweeper reacts promptly to a fast-forwarded clock without
// real sleeps approaching the TTL.
func WithSweepInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.sweepInterval = d
		}
	}
}

// WithRandReader overrides the entropy source used to mint session ids (for
// tests that need to force a generation failure). A nil reader is ignored.
func WithRandReader(read func([]byte) (int, error)) Option {
	return func(m *Manager) {
		if read != nil {
			m.randRead = read
		}
	}
}

// DefaultTTL is the default idle timeout stamped onto new sessions when no TTL
// is configured.
const DefaultTTL = 30 * time.Minute

// NewManager constructs a Manager over the given stores. Both stores are
// required; the package's in-memory implementations are the standalone default.
func NewManager(sessions SessionStore, history HistoryStore, opts ...Option) *Manager {
	m := &Manager{
		log:      slog.Default(),
		sessions: sessions,
		history:  history,
		now:      time.Now,
		ttl:      DefaultTTL,
		randRead: rand.Read,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, o := range opts {
		o(m)
	}
	if m.sweepInterval <= 0 {
		// Wake several times per TTL so an expired session is reaped well within a
		// TTL of going idle, without a tight busy-loop.
		m.sweepInterval = m.ttl / 4
		if m.sweepInterval <= 0 {
			m.sweepInterval = DefaultTTL / 4
		}
	}
	return m
}

// newID mints an unguessable session id ("sess_" + 128 bits of crypto/rand hex).
// A rand failure (effectively impossible on a healthy host) is surfaced rather
// than silently producing a weak id.
func (m *Manager) newID() (string, error) {
	var b [16]byte
	if _, err := m.randRead(b[:]); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	return idPrefix + hex.EncodeToString(b[:]), nil
}

// Create mints a new active session owned by ownerKeyID targeting model. It
// returns a copy of the created session, including its stable, unguessable id.
func (m *Manager) Create(_ context.Context, ownerKeyID, model string) (Session, error) {
	if ownerKeyID == "" {
		return Session{}, fmt.Errorf("session: empty owner key id")
	}
	id, err := m.newID()
	if err != nil {
		return Session{}, err
	}
	now := m.now()
	s := Session{
		ID:           id,
		OwnerKeyID:   ownerKeyID,
		Model:        model,
		CreatedAt:    now,
		LastActiveAt: now,
		TTL:          m.ttl,
		Status:       StatusActive,
	}
	if err := m.sessions.Put(s); err != nil {
		return Session{}, err
	}
	m.log.Debug("session created", "session", s.ID, "key_id", ownerKeyID, "model", model)
	return s.clone(), nil
}

// ownedActive fetches the session and enforces ownership, returning
// ErrSessionNotFound for both a missing session and one owned by another key (no
// existence leak). A session that has already idled out as of now is treated as
// gone (the sweeper will delete it) and also returns ErrSessionNotFound.
func (m *Manager) ownedActive(id, ownerKeyID string, now time.Time) (Session, error) {
	s, err := m.sessions.Get(id)
	if err != nil {
		return Session{}, ErrSessionNotFound
	}
	if s.OwnerKeyID != ownerKeyID {
		return Session{}, ErrSessionNotFound
	}
	if s.expired(now) {
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

// Resume returns the owner's session and touches its LastActiveAt to now,
// keeping it alive for another idle window. Returns ErrSessionNotFound if the
// session is missing, not owned by ownerKeyID, or already expired.
func (m *Manager) Resume(_ context.Context, id, ownerKeyID string) (Session, error) {
	now := m.now()
	s, err := m.ownedActive(id, ownerKeyID, now)
	if err != nil {
		return Session{}, err
	}
	s.LastActiveAt = now
	s.Status = StatusActive
	if err := m.sessions.Put(s); err != nil {
		return Session{}, err
	}
	return s.clone(), nil
}

// Get returns a copy of the owner's session without touching its activity
// timestamp. Returns ErrSessionNotFound if missing, not owned, or expired.
func (m *Manager) Get(_ context.Context, id, ownerKeyID string) (Session, error) {
	s, err := m.ownedActive(id, ownerKeyID, m.now())
	if err != nil {
		return Session{}, err
	}
	return s.clone(), nil
}

// Delete removes the owner's session and its history. Returns ErrSessionNotFound
// if the session is missing or not owned by ownerKeyID (so a caller cannot probe
// for, or delete, another owner's session).
func (m *Manager) Delete(_ context.Context, id, ownerKeyID string) error {
	// An expired-but-not-yet-swept session is still deletable by its owner, so do
	// not gate Delete on expiry here.
	s, err := m.sessions.Get(id)
	if err != nil {
		return ErrSessionNotFound
	}
	if s.OwnerKeyID != ownerKeyID {
		return ErrSessionNotFound
	}
	return m.deleteSession(id)
}

// deleteSession removes a session and its history. History is deleted first so a
// crash between the two leaves no session pointing at vanished history; an
// orphaned history with no session is harmless (it is purged on the next load or
// can be re-deleted).
func (m *Manager) deleteSession(id string) error {
	if err := m.history.DeleteBySession(id); err != nil {
		return err
	}
	return m.sessions.Delete(id)
}

// Bind records the worker a session is affinity-bound to (#34): the worker that
// holds the conversation's warm KV cache, which the scheduler then prefers for
// subsequent turns. It is owner-checked (ErrSessionNotFound for a missing,
// not-owned, or expired session, so existence never leaks), sets BoundWorkerID,
// touches LastActiveAt (a routed turn is activity), and persists. It returns a
// copy of the updated session. The server calls it after the first successful
// dispatch (first-turn binding) and again whenever the chosen worker differs
// from the bound one (rebind after the bound worker drains/evicts/goes stale).
func (m *Manager) Bind(_ context.Context, id, ownerKeyID, workerID string) (Session, error) {
	now := m.now()
	s, err := m.ownedActive(id, ownerKeyID, now)
	if err != nil {
		return Session{}, err
	}
	s.BoundWorkerID = workerID
	s.LastActiveAt = now
	if err := m.sessions.Put(s); err != nil {
		return Session{}, err
	}
	m.log.Debug("session bound", "session", s.ID, "key_id", ownerKeyID, "worker", workerID)
	return s.clone(), nil
}

// AppendTurn appends a chat turn to the owner's session history and touches the
// session's LastActiveAt (a turn is activity, so it keeps the session alive).
// Returns ErrSessionNotFound if missing, not owned, or expired. The history
// store enforces the configured turn/byte caps.
func (m *Manager) AppendTurn(_ context.Context, id, ownerKeyID string, turn types.Message) error {
	now := m.now()
	s, err := m.ownedActive(id, ownerKeyID, now)
	if err != nil {
		return err
	}
	if err := m.history.Append(id, turn); err != nil {
		return err
	}
	s.LastActiveAt = now
	return m.sessions.Put(s)
}

// History returns a copy of the owner's session history (oldest turn first).
// Returns ErrSessionNotFound if the session is missing, not owned, or expired.
func (m *Manager) History(_ context.Context, id, ownerKeyID string) ([]types.Message, error) {
	if _, err := m.ownedActive(id, ownerKeyID, m.now()); err != nil {
		return nil, err
	}
	return m.history.Get(id)
}

// Start launches the background idle-expiry sweeper. It is idempotent and safe
// to call before any sessions exist; Close stops it. The cmd layer (#36) calls
// Start at boot and Close on shutdown.
func (m *Manager) Start() {
	m.startOnce.Do(func() {
		go m.sweepLoop()
	})
}

// Close stops the sweeper and waits for it to exit. Safe to call once; further
// calls are no-ops. It is idempotent even if Start was never called.
func (m *Manager) Close() error {
	// Ensure the loop is running so the wait below cannot block forever.
	m.Start()
	select {
	case <-m.stop:
		// already closed
	default:
		close(m.stop)
	}
	<-m.done
	return nil
}

// sweepLoop reaps idle-expired sessions on a wall-clock ticker, using the
// injected clock for the expiry decision so tests fast-forward instead of
// sleeping. It mirrors the server's evictLoop lifecycle.
func (m *Manager) sweepLoop() {
	defer close(m.done)
	ticker := time.NewTicker(m.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweepExpired()
		}
	}
}

// sweepExpired deletes every session that has idled out as of now, along with
// its history. Errors are logged and skipped so one bad delete cannot wedge the
// sweep.
func (m *Manager) sweepExpired() {
	now := m.now()
	sessions, err := m.sessions.List()
	if err != nil {
		m.log.Warn("session sweep: list failed", "err", err)
		return
	}
	for _, s := range sessions {
		if !s.expired(now) {
			continue
		}
		if err := m.deleteSession(s.ID); err != nil {
			m.log.Warn("session sweep: delete failed", "session", s.ID, "err", err)
			continue
		}
		m.log.Info("session expired", "session", s.ID, "key_id", s.OwnerKeyID,
			"idle", now.Sub(s.LastActiveAt).String())
	}
}
