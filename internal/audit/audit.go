// Package audit is the immutable, append-only audit log for agent-gpu's admin
// (management) API (#90). Every administrative WRITE — key create/rotate/revoke,
// permission and quota changes, worker drain, and any future mutation — records
// one Entry: who did it (the actor key id), what they did (the op), to which
// resource, the before/after projection of the affected object, the request
// correlation id, and the outcome. The log is the tamper-evidence trail an
// operator (and a later queryable endpoint, #91) reads to answer "who changed
// what, when".
//
// # Secret hygiene
//
// The before/after snapshots are RedactedValues: a map of only safe,
// non-sensitive fields. Secret material (an APIKey's SecretHash/Salt, a token)
// is NEVER placed in an Entry — the recording call sites project through the
// admin metadata view, and Entry additionally implements slog.LogValuer so even
// an accidental log of a whole Entry cannot leak. There is no field on Entry
// that can hold a secret.
//
// # Append-only + durability
//
// The store is append-only: there is no update or delete. It is in-memory with
// a periodic file checkpoint (atomic temp+rename), mirroring the quota and
// session stores — so it reloads on restart and a crash loses at most one
// checkpoint interval of entries. No database is involved.
package audit

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Outcome is the result of an audited operation: whether the write succeeded or
// failed. It is a small closed vocabulary so the log is filterable.
type Outcome string

const (
	// OutcomeSuccess marks a write that completed and took effect.
	OutcomeSuccess Outcome = "success"
	// OutcomeFailure marks a write that was attempted but did not take effect
	// (validation rejected it, the target was missing, or the backend errored).
	OutcomeFailure Outcome = "failure"
)

// Entry is one immutable record of an administrative write. It is value-typed
// and copied in and out of the store so a caller can never mutate a stored
// record. Before/After are nil for operations with no meaningful prior/posterior
// object (e.g. a create has no Before; a failed revoke has no After).
type Entry struct {
	// Time is when the operation was recorded (UTC).
	Time time.Time `json:"time"`
	// Actor is the opaque id of the API key that performed the operation. It is
	// never a secret (the key id is the public identifier), and it is "" only if
	// the write somehow ran without an authenticated key on context (defensive).
	Actor string `json:"actor"`
	// Op is the operation name, e.g. "key.create", "key.revoke",
	// "key.permissions", "key.quota", "key.rotate", "worker.drain".
	Op string `json:"op"`
	// Target is the id of the resource the operation acted on (a key id, a worker
	// id). It is "" for an operation that targets no single resource.
	Target string `json:"target"`
	// Before is the redacted projection of the target object before the change,
	// or nil when there is no prior object (a create, or a no-op failure).
	Before RedactedValues `json:"before,omitempty"`
	// After is the redacted projection of the target object after the change, or
	// nil when there is no resulting object (a delete/revoke, or a failure).
	After RedactedValues `json:"after,omitempty"`
	// RequestID is the request correlation id (the X-Request-Id echoed to the
	// client and threaded through the logs), tying an audit entry to its request.
	RequestID string `json:"request_id"`
	// Outcome is whether the write succeeded or failed.
	Outcome Outcome `json:"outcome"`
}

// RedactedValues is a before/after snapshot: a map of only safe, non-sensitive
// fields of an audited object. It is deliberately a plain map (not a typed
// struct) so the same Entry shape records any resource, and deliberately NEVER
// carries secret material — call sites build it from the admin metadata
// projection, which omits SecretHash/Salt. It implements no marshaling magic; a
// nil map omits the field via the omitempty tags above.
type RedactedValues map[string]any

// clone returns a shallow copy of v so a stored snapshot cannot be mutated
// through the caller's reference. The values are scalars/strings/slices built by
// the projection helpers and are not retained by the caller after recording, so
// a shallow copy of the map is sufficient to isolate the stored record.
func (v RedactedValues) clone() RedactedValues {
	if v == nil {
		return nil
	}
	out := make(RedactedValues, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}

// LogValue implements slog.LogValuer so logging a whole Entry can never widen
// the audit surface unexpectedly: it emits only the safe scalar fields and the
// before/after key NAMES (not their values), so even an accidental
// slog.Any("entry", e) stays free of any field value. The before/after VALUES
// are intentionally elided from the log projection (they live in the audit
// store, not the operational log).
func (e Entry) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Time("time", e.Time),
		slog.String("actor", e.Actor),
		slog.String("op", e.Op),
		slog.String("target", e.Target),
		slog.String("request_id", e.RequestID),
		slog.String("outcome", string(e.Outcome)),
	)
}

// clone returns a deep-enough copy of the entry: the Before/After maps are
// copied so a stored record is isolated from the caller's. Scalar fields are
// value-copied by the struct copy.
func (e Entry) clone() Entry {
	out := e
	out.Before = e.Before.clone()
	out.After = e.After.clone()
	return out
}

// Filter narrows a List query. A zero Filter matches every entry. Fields are
// ANDed: an entry must match every non-empty field. Time bounds are inclusive of
// Since and exclusive of Until (a half-open window), matching the usual
// convention; a zero time bound is unbounded on that side.
type Filter struct {
	// Actor, if non-empty, matches entries by exactly this actor key id.
	Actor string
	// Op, if non-empty, matches entries with exactly this op.
	Op string
	// Target, if non-empty, matches entries acting on exactly this resource id.
	Target string
	// Since, if non-zero, excludes entries recorded before it (inclusive).
	Since time.Time
	// Until, if non-zero, excludes entries recorded at or after it (exclusive).
	Until time.Time
}

func (f Filter) matches(e Entry) bool {
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Op != "" && e.Op != f.Op {
		return false
	}
	if f.Target != "" && e.Target != f.Target {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.Time.Before(f.Until) {
		return false
	}
	return true
}

// MemoryStore is an in-memory, concurrency-safe, append-only audit log. Entries
// are held in insertion order; List returns them filtered and sorted, and a
// periodic Checkpoint/LoadCheckpoint pair persists them to a JSON file (atomic
// temp+rename) so the log survives restarts and a crash loses at most one
// checkpoint interval. There is deliberately no update or delete: the log is
// immutable once written.
//
// A cap bounds memory: when the number of entries would exceed maxEntries, the
// oldest are dropped (the audit log is a rolling window, not unbounded growth).
// A non-positive cap means unbounded.
type MemoryStore struct {
	mu      sync.RWMutex
	entries []Entry
	max     int
}

// NewMemoryStore returns an empty audit store bounded to maxEntries (non-positive
// = unbounded). The bound is a rolling window: the oldest entries are evicted
// first when the cap is exceeded.
func NewMemoryStore(maxEntries int) *MemoryStore {
	return &MemoryStore{max: maxEntries}
}

// Append records one entry. It stamps the entry's Time with now if the caller
// left it zero, copies the before/after maps so the stored record is isolated,
// and evicts the oldest entries if the cap is exceeded. It never returns an
// error (an in-memory append cannot fail) but keeps the error return so a
// future durable backend can slot in without a signature change.
func (m *MemoryStore) Append(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	stored := e.clone()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, stored)
	if m.max > 0 && len(m.entries) > m.max {
		// Drop the oldest overflow. Re-slice onto a fresh backing array so the
		// dropped entries are not pinned in memory by the retained slice header.
		drop := len(m.entries) - m.max
		trimmed := make([]Entry, m.max)
		copy(trimmed, m.entries[drop:])
		m.entries = trimmed
	}
	return nil
}

// List returns the entries matching filter, newest first, capped at limit
// (limit <= 0 means no cap). Returned entries are copies, so a caller cannot
// mutate the stored log. It is the read seam the queryable endpoint (#91) builds
// on; here it backs the store-level tests and the checkpoint round-trip.
func (m *MemoryStore) List(filter Filter, limit int) []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if filter.matches(e) {
			out = append(out, e.clone())
		}
	}
	// Newest first: stable sort by time descending, falling back to insertion
	// order (already chronological) for equal timestamps.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Time.After(out[j].Time)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Len returns the number of stored entries. It is primarily for tests and the
// cap assertions.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}
