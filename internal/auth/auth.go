// Package auth implements the API-key lifecycle and authentication engine for
// agent-gpu. A key is the unit of identity that permissions and quotas later
// attach to (#3, #6).
//
// Token format: agpu_<keyid>_<secret>
//
//   - keyid  is a short, public, crypto/rand identifier stored in plaintext;
//     it IS the store.APIKey.ID, giving O(1) lookup (no scan-and-hash).
//   - secret is a >=256-bit crypto/rand value shown ONCE at creation and never
//     stored. Only a per-key salted SHA-256 hash (SHA-256(salt||secret)) and
//     its salt are persisted, so a leaked store cannot mint usable tokens.
//
// Salted SHA-256 (not bcrypt/argon2) is a deliberate choice: this is an
// inference gateway where every request authenticates, and the secret has
// >=256 bits of entropy, so a slow KDF buys no meaningful brute-force margin
// while adding 50-100ms to the hot path.
//
// This package owns ONLY the engine + lifecycle. HTTP endpoints (#4) and
// request-path middleware (#13) are out of scope; ErrUnauthenticated is the
// typed seam they map to HTTP 401.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

// Prefix is the token namespace prefix.
const Prefix = "agpu"

const (
	// keyIDBytes is the entropy of the public key id (8 bytes = 16 hex chars).
	keyIDBytes = 8
	// secretBytes is the entropy of the secret (32 bytes = 256 bits).
	secretBytes = 32
	// saltBytes is the per-key salt length.
	saltBytes = 32
)

// ErrUnauthenticated is returned by Authenticate for any failure that must not
// distinguish an unknown key id from a wrong secret from a revoked key (no user
// enumeration). Callers (#4/#13) map it to HTTP 401.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Service is the API-key lifecycle and authentication engine over a Store.
//
// The mutex serializes read-modify-write lifecycle operations (Rotate, and the
// usage-counter bump on successful auth) so a Get+Put against the store cannot
// race (TOCTOU). The store's own locking only protects individual calls.
type Service struct {
	mu    sync.Mutex
	store store.Store
	now   func() time.Time
	// randRead is the entropy source, injectable for tests. Defaults to
	// crypto/rand.Read.
	randRead func([]byte) (int, error)
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the time source (for tests).
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// withRand overrides the entropy source (for tests). Unexported: production
// callers must use crypto/rand.
func withRand(read func([]byte) (int, error)) Option {
	return func(s *Service) { s.randRead = read }
}

// NewService returns a Service backed by st.
func NewService(st store.Store, opts ...Option) *Service {
	s := &Service{
		store:    st,
		now:      time.Now,
		randRead: rand.Read,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create generates a new key (id + secret + salt), persists only its salted
// hash, and returns the one-time plaintext token. The plaintext is never stored
// and cannot be recovered afterward. The key is created with no roles or
// model lists; use CreateWithPermissions (or SetPermissions) to grant access.
func (s *Service) Create(ctx context.Context, name string) (token string, key store.APIKey, err error) {
	return s.CreateWithPermissions(ctx, name, Permissions{})
}

// CreateWithPermissions is Create with initial roles and allow/deny lists
// applied atomically at creation, so a key can be born with access rather than
// requiring a follow-up SetPermissions call.
func (s *Service) CreateWithPermissions(ctx context.Context, name string, perms Permissions) (token string, key store.APIKey, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, secret, salt, hash, err := s.mint()
	if err != nil {
		return "", store.APIKey{}, err
	}

	rec := store.APIKey{
		ID:          id,
		Name:        name,
		Prefix:      Prefix,
		SecretHash:  hash,
		Salt:        salt,
		CreatedAt:   s.now().UTC(),
		Roles:       perms.Roles,
		AdminScopes: perms.AdminScopes,
		AllowModels: perms.AllowModels,
		DenyModels:  perms.DenyModels,
	}
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return "", store.APIKey{}, fmt.Errorf("auth: persist key: %w", err)
	}
	return formatToken(id, secret), rec, nil
}

// List returns all stored keys (metadata only; no secrets are ever stored).
func (s *Service) List(ctx context.Context) ([]store.APIKey, error) {
	return s.store.ListAPIKeys(ctx)
}

// Get returns the key with the given id, or store.ErrNotFound. It is the
// single-key inspection seam for the admin API (#4); like List it returns only
// metadata (the plaintext secret is never stored and so can never be returned).
func (s *Service) Get(ctx context.Context, id string) (store.APIKey, error) {
	return s.store.GetAPIKey(ctx, id)
}

// Revoke marks the key revoked. Revoked keys never authenticate. Revoking an
// unknown key returns store.ErrNotFound. Revoking an already-revoked key is a
// no-op success.
func (s *Service) Revoke(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return err
	}
	if rec.Revoked() {
		return nil
	}
	t := s.now().UTC()
	rec.RevokedAt = &t
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return fmt.Errorf("auth: persist revoke: %w", err)
	}
	return nil
}

// Permissions are the role, admin-scope, and per-model allow/deny lists set on a
// key. They are interpreted by internal/authz; this package only persists them.
type Permissions struct {
	Roles       []string
	AdminScopes []string
	AllowModels []string
	DenyModels  []string
}

// SetPermissions replaces a key's roles, admin scopes, and allow/deny lists with
// the supplied values (a full replace, not a merge), preserving the key's
// identity and secret. It is the management seam used by the `key perms` CLI and
// the admin HTTP endpoints (#4/#90). Setting permissions on an unknown key
// returns store.ErrNotFound; nil slices clear the corresponding list.
//
// Because authorization reads the key fresh from the store on every check
// (internal/authz caches nothing), a change here takes effect immediately with
// no restart.
func (s *Service) SetPermissions(ctx context.Context, id string, perms Permissions) (store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return store.APIKey{}, err
	}
	rec.Roles = perms.Roles
	rec.AdminScopes = perms.AdminScopes
	rec.AllowModels = perms.AllowModels
	rec.DenyModels = perms.DenyModels
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return store.APIKey{}, fmt.Errorf("auth: persist permissions: %w", err)
	}
	return rec, nil
}

// SetLimits replaces a key's quota limits (#5), preserving the key's identity,
// secret, and permissions. Passing a nil *store.Limits clears the per-key
// override so the key falls back to the global quota defaults; a non-nil value
// overrides them (a zero field within it meaning "unlimited"). Setting limits
// on an unknown key returns store.ErrNotFound.
//
// Because the quota engine reads the key's limits from the store on each check,
// a change here takes effect immediately with no restart. It is the management
// seam used by the `key quota set` CLI until the admin HTTP endpoints (#4)
// exist.
func (s *Service) SetLimits(ctx context.Context, id string, limits *store.Limits) (store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return store.APIKey{}, err
	}
	if limits != nil {
		l := *limits
		rec.Limits = &l
	} else {
		rec.Limits = nil
	}
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return store.APIKey{}, fmt.Errorf("auth: persist limits: %w", err)
	}
	return rec, nil
}

// Rotate atomically replaces a key's secret and salt while preserving its id
// (and thus all identity/permissions/quotas attached to it). The old secret
// stops verifying immediately. Returns the new one-time plaintext token. A
// revoked key cannot be rotated.
func (s *Service) Rotate(ctx context.Context, id string) (token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return "", err
	}
	if rec.Revoked() {
		return "", fmt.Errorf("auth: cannot rotate revoked key: %w", ErrUnauthenticated)
	}

	secret, salt, hash, err := s.mintSecret()
	if err != nil {
		return "", err
	}
	rec.SecretHash = hash
	rec.Salt = salt
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return "", fmt.Errorf("auth: persist rotate: %w", err)
	}
	return formatToken(id, secret), nil
}

// Authenticate parses a token, looks up the key by its (plaintext) id,
// constant-time compares the salted hash, rejects revoked keys, and on success
// atomically bumps UsageCount and LastUsedAt. It returns the same
// ErrUnauthenticated for unknown-id, wrong-secret, malformed, and revoked
// inputs so callers cannot enumerate keys.
func (s *Service) Authenticate(ctx context.Context, token string) (store.APIKey, error) {
	id, secret, ok := parseToken(token)
	if !ok {
		return store.APIKey{}, ErrUnauthenticated
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.APIKey{}, ErrUnauthenticated
		}
		return store.APIKey{}, fmt.Errorf("auth: lookup key: %w", err)
	}

	want := rec.SecretHash
	got := hashSecret(rec.Salt, secret)
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return store.APIKey{}, ErrUnauthenticated
	}
	if rec.Revoked() {
		return store.APIKey{}, ErrUnauthenticated
	}

	rec.UsageCount++
	rec.LastUsedAt = s.now().UTC()
	if err := s.store.PutAPIKey(ctx, rec); err != nil {
		return store.APIKey{}, fmt.Errorf("auth: persist usage: %w", err)
	}
	return rec, nil
}

// mint generates a fresh id + secret + salt and the secret's salted hash.
func (s *Service) mint() (id, secret string, salt, hash []byte, err error) {
	idBytes, err := s.randBytes(keyIDBytes)
	if err != nil {
		return "", "", nil, nil, err
	}
	secret, salt, hash, err = s.mintSecret()
	if err != nil {
		return "", "", nil, nil, err
	}
	return hex.EncodeToString(idBytes), secret, salt, hash, nil
}

// mintSecret generates a fresh secret + salt and the secret's salted hash.
func (s *Service) mintSecret() (secret string, salt, hash []byte, err error) {
	secretBuf, err := s.randBytes(secretBytes)
	if err != nil {
		return "", nil, nil, err
	}
	salt, err = s.randBytes(saltBytes)
	if err != nil {
		return "", nil, nil, err
	}
	secret = hex.EncodeToString(secretBuf)
	hash = hashSecret(salt, secret)
	return secret, salt, hash, nil
}

// randBytes returns n cryptographically-random bytes.
func (s *Service) randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := s.randRead(b); err != nil {
		return nil, fmt.Errorf("auth: read entropy: %w", err)
	}
	return b, nil
}

// hashSecret computes SHA-256(salt || secret).
func hashSecret(salt []byte, secret string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(secret))
	return h.Sum(nil)
}

// formatToken assembles the wire token: agpu_<keyid>_<secret>.
func formatToken(id, secret string) string {
	return Prefix + "_" + id + "_" + secret
}

// parseToken splits a token into its id and secret. It returns ok=false for any
// structurally invalid token (wrong prefix, missing parts, empty fields).
func parseToken(token string) (id, secret string, ok bool) {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	if parts[0] != Prefix || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}
