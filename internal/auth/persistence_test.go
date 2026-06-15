package auth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// TestNoPlaintextSecretPersisted covers AC3: the persisted store contains no
// plaintext secret (or token) — only salt + hash. It drives a real Service
// against the file-backed store and inspects the raw bytes on disk.
func TestNoPlaintextSecretPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")

	st, err := store.NewFile(path)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := auth.NewService(st)
	token, _, err := svc.Create(ctx, "secret-leak-check")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Authenticate + rotate so usage updates and a rotation are also flushed.
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	rotated, err := svc.Rotate(ctx, strings.SplitN(token, "_", 3)[1])
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	disk := string(raw)

	for _, tok := range []string{token, rotated} {
		secret := strings.SplitN(tok, "_", 3)[2]
		if strings.Contains(disk, secret) {
			t.Fatal("persisted store contains a plaintext secret")
		}
		if strings.Contains(disk, tok) {
			t.Fatal("persisted store contains a plaintext token")
		}
	}
}
