package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// keySpec accumulates the options for a built or minted key. It separates the
// permission/limit intent from the two construction paths: Key builds a bare
// store.APIKey value, while MintKey drives a real auth.Service so the resulting
// token authenticates.
type keySpec struct {
	name        string
	roles       []string
	allowModels []string
	denyModels  []string
	limits      *store.Limits
}

// KeyOption configures a built or minted key. The same options feed both Key and
// MintKey, so a test expresses "a user key for llama3 capped at 1 RPM" once and
// uses it with whichever construction path it needs.
type KeyOption func(*keySpec)

// WithKeyName sets the key's human-readable name (default "test").
func WithKeyName(name string) KeyOption {
	return func(s *keySpec) { s.name = name }
}

// WithRoles sets the key's granted roles (e.g. authz.RoleUser, authz.RoleAdmin).
func WithRoles(roles ...string) KeyOption {
	return func(s *keySpec) { s.roles = roles }
}

// WithAllowModels sets the key's per-key model allow-list.
func WithAllowModels(models ...string) KeyOption {
	return func(s *keySpec) { s.allowModels = models }
}

// WithDenyModels sets the key's per-key model deny-list (deny wins over allow).
func WithDenyModels(models ...string) KeyOption {
	return func(s *keySpec) { s.denyModels = models }
}

// WithLimits sets the key's full quota limits (a copy is stored).
func WithLimits(l store.Limits) KeyOption {
	return func(s *keySpec) {
		c := l
		s.limits = &c
	}
}

// WithRPM sets only the per-key requests-per-minute cap, leaving other limit
// dimensions unlimited. It generalizes the common "cap RPM" fixture.
func WithRPM(rpm uint64) KeyOption {
	return func(s *keySpec) {
		if s.limits == nil {
			s.limits = &store.Limits{}
		}
		s.limits.RPM = rpm
	}
}

// specFromOptions applies defaults then opts.
func specFromOptions(opts []KeyOption) keySpec {
	s := keySpec{name: "test"}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// permissions renders the spec's role/allow/deny intent as an auth.Permissions.
func (s keySpec) permissions() auth.Permissions {
	return auth.Permissions{
		Roles:       s.roles,
		AllowModels: s.allowModels,
		DenyModels:  s.denyModels,
	}
}

// Key builds a bare store.APIKey value (no real secret) carrying the requested
// name, roles, allow/deny lists, and limits. Use it for store/authz tests that
// inspect or authorize a key record directly; use MintKey when the token must
// authenticate through auth.Service.
func Key(opts ...KeyOption) store.APIKey {
	s := specFromOptions(opts)
	return store.APIKey{
		ID:          "test-key",
		Name:        s.name,
		Prefix:      auth.Prefix,
		CreatedAt:   time.Unix(0, 0).UTC(),
		Roles:       s.roles,
		AllowModels: s.allowModels,
		DenyModels:  s.denyModels,
		Limits:      s.limits,
	}
}

// MintKey creates a real key through svc with the requested permissions and
// limits, returning the persisted record and its one-time plaintext token. It
// generalizes the per-package mintUserKey/mustKey/authedKey helpers: any role
// set, allow/deny lists, and limits are expressible via options.
//
// It fails the test (t.Fatalf) on any creation or limit error, so callers can use
// the returned values directly.
func MintKey(t *testing.T, svc *auth.Service, opts ...KeyOption) (store.APIKey, string) {
	t.Helper()
	s := specFromOptions(opts)
	ctx := context.Background()

	token, created, err := svc.CreateWithPermissions(ctx, s.name, s.permissions())
	if err != nil {
		t.Fatalf("testutil: create key: %v", err)
	}
	if s.limits != nil {
		updated, err := svc.SetLimits(ctx, created.ID, s.limits)
		if err != nil {
			t.Fatalf("testutil: set limits: %v", err)
		}
		created = updated
	}
	return created, token
}

// MintToken is MintKey when only the bearer token is needed (the common case for
// HTTP-level tests). It is a thin wrapper that discards the record.
func MintToken(t *testing.T, svc *auth.Service, opts ...KeyOption) string {
	t.Helper()
	_, token := MintKey(t, svc, opts...)
	return token
}
