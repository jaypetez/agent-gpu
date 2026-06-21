package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })
	return NewService(st)
}

// TestCreateAndAuthenticate covers AC1 (a created key authenticates) and the
// token shape.
func TestCreateAndAuthenticate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.Create(ctx, "agent-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(token, Prefix+"_") {
		t.Fatalf("token missing prefix: %q", token)
	}
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[1] != key.ID {
		t.Fatalf("token id segment %v does not match key id %q", parts, key.ID)
	}
	if key.Name != "agent-1" || key.Prefix != Prefix {
		t.Fatalf("unexpected key metadata: %+v", key)
	}

	got, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != key.ID {
		t.Fatalf("authenticated id %q want %q", got.ID, key.ID)
	}
}

// TestRevokeRejects covers AC1 (a revoked key is rejected with ErrUnauthenticated).
func TestRevokeRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.Create(ctx, "to-revoke")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for revoked key, got %v", err)
	}

	// Revoking again is a no-op success; revoking an unknown key is ErrNotFound.
	if err := svc.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("re-revoke should be no-op, got %v", err)
	}
	if err := svc.Revoke(ctx, "deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke unknown: want ErrNotFound, got %v", err)
	}
}

// TestRotate covers AC2: rotating invalidates the old secret and issues a new one.
func TestRotate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	oldToken, key, err := svc.Create(ctx, "rot")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newToken, err := svc.Rotate(ctx, key.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newToken == oldToken {
		t.Fatalf("rotate returned the same token")
	}

	// Identity (key id) is preserved across rotation.
	oldParts := strings.SplitN(oldToken, "_", 3)
	newParts := strings.SplitN(newToken, "_", 3)
	if oldParts[1] != newParts[1] {
		t.Fatalf("rotate changed key id: %q -> %q", oldParts[1], newParts[1])
	}

	// Old secret no longer verifies; new secret does.
	if _, err := svc.Authenticate(ctx, oldToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old token should be rejected, got %v", err)
	}
	if _, err := svc.Authenticate(ctx, newToken); err != nil {
		t.Fatalf("new token should authenticate, got %v", err)
	}

	// Cannot rotate a revoked key.
	if err := svc.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Rotate(ctx, key.ID); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("rotate revoked: want ErrUnauthenticated, got %v", err)
	}
}

// TestWrongSecret covers AC5: a wrong secret for a valid id is rejected with the
// same error as an unknown id (no enumeration).
func TestWrongSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.Create(ctx, "k")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	parts := strings.SplitN(token, "_", 3)
	tampered := Prefix + "_" + key.ID + "_deadbeefdeadbeef"
	if tampered == token {
		t.Fatal("tampered token unexpectedly equal")
	}
	if _, err := svc.Authenticate(ctx, tampered); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong secret: want ErrUnauthenticated, got %v", err)
	}
	_ = parts
}

// TestUnknownID covers AC5: an unknown id returns ErrUnauthenticated (same as
// wrong secret).
func TestUnknownID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)
	if _, err := svc.Authenticate(ctx, Prefix+"_abcdef0123456789_somesecret"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown id: want ErrUnauthenticated, got %v", err)
	}
}

// TestMalformedToken covers AC5: structurally invalid tokens are rejected.
func TestMalformedToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	cases := []string{
		"",
		"garbage",
		"agpu_only-two",
		"wrong_id_secret", // bad prefix
		"agpu__secret",    // empty id
		"agpu_id_",        // empty secret
		"agpu_id",         // missing secret part
	}
	for _, tok := range cases {
		if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("malformed %q: want ErrUnauthenticated, got %v", tok, err)
		}
	}
}

// TestUsageTracking covers AC4: usage count and last-used increment on each
// successful authentication and are persisted.
func TestUsageTracking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	var clock int64
	svc := NewService(st, WithClock(func() time.Time {
		clock++
		return time.Unix(clock, 0).UTC()
	}))

	token, key, err := svc.Create(ctx, "usage")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if _, err := svc.Authenticate(ctx, token); err != nil {
			t.Fatalf("auth %d: %v", i, err)
		}
		rec, err := st.GetAPIKey(ctx, key.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rec.UsageCount != uint64(i) {
			t.Fatalf("usage count = %d, want %d", rec.UsageCount, i)
		}
		if rec.LastUsedAt.IsZero() {
			t.Fatalf("last-used not set after auth %d", i)
		}
	}

	// A failed auth must NOT bump usage.
	if _, err := svc.Authenticate(ctx, Prefix+"_"+key.ID+"_bad"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected failure, got %v", err)
	}
	rec, _ := st.GetAPIKey(ctx, key.ID)
	if rec.UsageCount != 3 {
		t.Fatalf("failed auth changed usage count to %d", rec.UsageCount)
	}
}

// TestExpiredKeyRejected covers #96 AC1: a key whose TTL (ExpiresAt) has passed
// fails authentication with ErrUnauthenticated (the same sentinel as
// revoked/unknown — no enumeration), while a not-yet-expired key authenticates.
// A controllable clock drives the expiry decision so there are no wall-clock
// sleeps.
func TestExpiredKeyRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	// A clock the test advances explicitly. Both Create (CreatedAt) and
	// Authenticate (the Expired check) read it via s.now().
	now := time.Unix(1_000, 0).UTC()
	svc := NewService(st, WithClock(func() time.Time { return now }))

	// Mint a key that expires at t=2000 (1000 seconds in the "future").
	expiry := time.Unix(2_000, 0).UTC()
	token, key, err := svc.CreateWithPermissions(ctx, "ttl", Permissions{ExpiresAt: &expiry})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.ExpiresAt == nil || !key.ExpiresAt.Equal(expiry) {
		t.Fatalf("created key ExpiresAt = %v, want %v", key.ExpiresAt, expiry)
	}

	// Before expiry: authenticates normally.
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("pre-expiry authenticate: want success, got %v", err)
	}

	// Exactly at the expiry instant the key is NOT yet expired (Expired uses a
	// strict After), so it still authenticates.
	now = expiry
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("at-expiry authenticate: want success (boundary is inclusive), got %v", err)
	}

	// One second past expiry: authentication fails with the shared sentinel.
	now = expiry.Add(time.Second)
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("post-expiry authenticate: want ErrUnauthenticated, got %v", err)
	}
}

// TestNonExpiringKeyAlwaysAuthenticates covers #96 AC1/AC3 backward-compat: a key
// created without an ExpiresAt (nil TTL) never expires — it still authenticates
// far in the "future" — proving a key without the new field behaves exactly as
// before.
func TestNonExpiringKeyAlwaysAuthenticates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	t.Cleanup(func() { _ = st.Close() })

	now := time.Unix(1_000, 0).UTC()
	svc := NewService(st, WithClock(func() time.Time { return now }))

	token, key, err := svc.Create(ctx, "no-ttl")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.ExpiresAt != nil {
		t.Fatalf("key created without a TTL should have nil ExpiresAt, got %v", key.ExpiresAt)
	}

	// Advance the clock far past any plausible expiry — a nil-TTL key still works.
	now = time.Unix(1_000_000_000, 0).UTC()
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("non-expiring key should always authenticate, got %v", err)
	}
}

// TestCreateWithExpiryRoundTrips covers #96 AC1/AC3: Owner/Team/ExpiresAt/
// CreatedBy supplied at creation are persisted and survive a fresh read from the
// store, and the ExpiresAt pointer is copied (not shared) so mutating the caller's
// time does not corrupt the stored record.
func TestCreateWithExpiryRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	expiry := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	_, key, err := svc.CreateWithPermissions(ctx, "rich", Permissions{
		Owner:     "alice@example.com",
		Team:      "platform",
		ExpiresAt: &expiry,
		CreatedBy: "admin_key_1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.store.GetAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Owner != "alice@example.com" || got.Team != "platform" || got.CreatedBy != "admin_key_1" {
		t.Fatalf("metadata not persisted: owner=%q team=%q created_by=%q", got.Owner, got.Team, got.CreatedBy)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiry) {
		t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, expiry)
	}

	// Mutating the local expiry variable must not affect the stored value (the
	// service normalized + copied the pointer).
	expiry = expiry.Add(24 * time.Hour)
	again, _ := svc.store.GetAPIKey(ctx, key.ID)
	if again.ExpiresAt.Equal(expiry) {
		t.Fatal("stored ExpiresAt aliases the caller's pointer (was mutated)")
	}
}

// TestSetPermissionsPreservesEnrichment covers #96 AC3: a permissions update
// (which replaces only roles/scopes/model lists) must NOT clear a key's
// Owner/Team/ExpiresAt/CreatedBy.
func TestSetPermissionsPreservesEnrichment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	expiry := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	_, key, err := svc.CreateWithPermissions(ctx, "rich", Permissions{
		Roles:     []string{"user"},
		Owner:     "bob",
		Team:      "infra",
		ExpiresAt: &expiry,
		CreatedBy: "admin_key_2",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := svc.SetPermissions(ctx, key.ID, Permissions{Roles: []string{"read-only"}})
	if err != nil {
		t.Fatalf("set permissions: %v", err)
	}
	if updated.Owner != "bob" || updated.Team != "infra" || updated.CreatedBy != "admin_key_2" {
		t.Errorf("enrichment lost after SetPermissions: %+v", updated)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(expiry) {
		t.Errorf("expiry lost after SetPermissions: %v", updated.ExpiresAt)
	}
}

// TestConcurrentRotateAuth covers AC5: concurrent rotate + authenticate never
// races or corrupts state, and authentication always succeeds with whatever the
// current valid token is.
func TestConcurrentRotateAuth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	_, key, err := svc.Create(ctx, "concurrent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	// Rotators racing with auth attempts on the same id.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Rotate(ctx, key.ID); err != nil {
				t.Errorf("rotate: %v", err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The token may be superseded by a concurrent rotation before we
			// authenticate, so the only invariant is: the result is either a
			// success or a clean ErrUnauthenticated — never a corrupted/wrapped
			// store error (which would indicate a torn read-modify-write).
			tok, rerr := svc.Rotate(ctx, key.ID)
			if rerr != nil {
				return
			}
			if _, aerr := svc.Authenticate(ctx, tok); aerr != nil && !errors.Is(aerr, ErrUnauthenticated) {
				t.Errorf("authenticate returned non-ErrUnauthenticated error: %v", aerr)
			}
		}()
	}
	wg.Wait()

	// State remains coherent: the key still exists and is usable after rotation.
	tok, err := svc.Rotate(ctx, key.ID)
	if err != nil {
		t.Fatalf("final rotate: %v", err)
	}
	if _, err := svc.Authenticate(ctx, tok); err != nil {
		t.Fatalf("final authenticate: %v", err)
	}
}

// TestConcurrentUsageCount covers AC4 under concurrency: N successful auths
// produce exactly N counted, with no lost updates.
func TestConcurrentUsageCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)

	token, key, err := svc.Create(ctx, "race")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Authenticate(ctx, token); err != nil {
				t.Errorf("auth: %v", err)
			}
		}()
	}
	wg.Wait()

	rec, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("expected 1 key, got %d", len(rec))
	}
	if rec[0].ID != key.ID {
		t.Fatalf("unexpected id %q", rec[0].ID)
	}
	if rec[0].UsageCount != n {
		t.Fatalf("usage count = %d, want %d (lost updates)", rec[0].UsageCount, n)
	}
}
