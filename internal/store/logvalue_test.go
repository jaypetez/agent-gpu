package store

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestAPIKeyLogValueOmitsSecrets proves the type-level redaction guarantee (#23):
// logging an APIKey through any slog handler renders only the safe identifying
// fields (id, name, roles, revoked) and NEVER the SecretHash or Salt — neither
// the field names nor the bytes in any encoding. This holds independently of the
// cmd-layer ReplaceAttr backstop, so even a subsystem logging an APIKey directly
// cannot leak the secret material.
func TestAPIKeyLogValueOmitsSecrets(t *testing.T) {
	t.Parallel()

	secretHash := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x11, 0x22, 0x33, 0x44}
	salt := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0x55, 0x66, 0x77, 0x88}
	now := time.Unix(0, 0).UTC()
	key := APIKey{
		ID:         "key-abc",
		Name:       "service-key",
		Prefix:     "agpu",
		SecretHash: secretHash,
		Salt:       salt,
		Roles:      []string{"user"},
		CreatedAt:  now,
	}

	var buf bytes.Buffer
	// A plain JSON handler with NO ReplaceAttr, so this asserts the guarantee comes
	// from LogValue alone, not from the cmd-layer redaction.
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("key loaded", "key", key)
	out := buf.String()

	// None of the secret material, in any encoding, may appear.
	for _, forbidden := range []string{
		base64.StdEncoding.EncodeToString(secretHash),
		base64.StdEncoding.EncodeToString(salt),
		hex.EncodeToString(secretHash),
		hex.EncodeToString(salt),
		string(secretHash),
		string(salt),
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("APIKey log output leaked secret material %q:\n%s", forbidden, out)
		}
	}
	// The secret-bearing field names must never be serialized.
	if strings.Contains(out, "SecretHash") || strings.Contains(out, "Salt") {
		t.Errorf("APIKey log output serialized a secret field name:\n%s", out)
	}
	// The safe fields survive so auth logging stays useful.
	for _, want := range []string{`"id":"key-abc"`, `"name":"service-key"`} {
		if !strings.Contains(out, want) {
			t.Errorf("APIKey log output dropped safe field %q:\n%s", want, out)
		}
	}
}

// TestAPIKeyLogValueRevokedFlag proves the revoked flag reflects RevokedAt.
func TestAPIKeyLogValueRevokedFlag(t *testing.T) {
	t.Parallel()

	live := APIKey{ID: "k1"}
	if v := groupField(t, live.LogValue(), "revoked"); v != false {
		t.Errorf("live key revoked = %v, want false", v)
	}

	ts := time.Unix(100, 0).UTC()
	revoked := APIKey{ID: "k2", RevokedAt: &ts}
	if v := groupField(t, revoked.LogValue(), "revoked"); v != true {
		t.Errorf("revoked key revoked = %v, want true", v)
	}
}

// groupField extracts a single attribute's resolved value from a group slog.Value.
func groupField(t *testing.T, v slog.Value, key string) any {
	t.Helper()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want group", v.Kind())
	}
	for _, a := range v.Group() {
		if a.Key == key {
			return a.Value.Any()
		}
	}
	t.Fatalf("group missing field %q", key)
	return nil
}
