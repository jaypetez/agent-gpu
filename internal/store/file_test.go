package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileCRUDPersistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "keys.json")

	f, err := NewFile(path)
	if err != nil {
		t.Fatalf("new file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.GetAPIKey(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	key := APIKey{ID: "k1", Name: "agent", SecretHash: []byte{1, 2, 3}, Salt: []byte{4, 5}}
	if err := f.PutAPIKey(ctx, key); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Reopen from disk: state must persist across processes.
	f2, err := NewFile(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	got, err := f2.GetAPIKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Name != "agent" || string(got.SecretHash) != string(key.SecretHash) {
		t.Fatalf("reopen mismatch: %+v", got)
	}

	if err := f2.DeleteAPIKey(ctx, "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := f2.GetAPIKey(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	// Deleting a missing key is not an error.
	if err := f2.DeleteAPIKey(ctx, "nope"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "secure")
	path := filepath.Join(dir, "keys.json")

	f, err := NewFile(path)
	if err != nil {
		t.Fatalf("new file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := f.PutAPIKey(context.Background(), APIKey{ID: "k1"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != fileMode {
		t.Fatalf("keys file perm = %o, want %o", perm, fileMode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != dirMode {
		t.Fatalf("keys dir perm = %o, want %o", perm, dirMode)
	}
}

func TestFileConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keys.json")
	f, err := NewFile(path)
	if err != nil {
		t.Fatalf("new file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			_ = f.PutAPIKey(ctx, APIKey{ID: id, Name: id})
			_, _ = f.GetAPIKey(ctx, id)
			_, _ = f.ListAPIKeys(ctx)
		}(i)
	}
	wg.Wait()
}
