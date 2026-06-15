package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemory()
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.GetAPIKey(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	key := APIKey{ID: "k1", Name: "agent"}
	if err := s.PutAPIKey(ctx, key); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetAPIKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != key.ID || got.Name != key.Name {
		t.Fatalf("got %+v want %+v", got, key)
	}

	list, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 key, got %d", len(list))
	}

	// Overwrite.
	if err := s.PutAPIKey(ctx, APIKey{ID: "k1", Name: "renamed"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = s.GetAPIKey(ctx, "k1")
	if got.Name != "renamed" {
		t.Fatalf("overwrite failed: %+v", got)
	}

	// Delete (missing delete is not an error).
	if err := s.DeleteAPIKey(ctx, "nope"); err != nil {
		t.Fatalf("delete missing should be nil, got %v", err)
	}
	if err := s.DeleteAPIKey(ctx, "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetAPIKey(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestPermissionFieldsDeepCopied verifies the new Roles/AllowModels/DenyModels
// slices round-trip through the store and are deep-copied by cloneAPIKey, so a
// caller mutating its copy cannot corrupt stored state.
func TestPermissionFieldsDeepCopied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemory()
	t.Cleanup(func() { _ = s.Close() })

	key := APIKey{
		ID:          "k1",
		Roles:       []string{"user"},
		AllowModels: []string{"llama3"},
		DenyModels:  []string{"bad"},
	}
	if err := s.PutAPIKey(ctx, key); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Mutating the caller's slices after Put must not affect stored state.
	key.Roles[0] = "admin"
	key.AllowModels[0] = "mistral"

	got, err := s.GetAPIKey(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Roles[0] != "user" || got.AllowModels[0] != "llama3" || got.DenyModels[0] != "bad" {
		t.Fatalf("store aliased caller slices: %+v", got)
	}

	// Mutating the returned copy must not affect a subsequent read either.
	got.Roles[0] = "admin"
	again, _ := s.GetAPIKey(ctx, "k1")
	if again.Roles[0] != "user" {
		t.Fatalf("GetAPIKey returned aliased slice: %+v", again)
	}
}

func TestMemoryConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemory()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			_ = s.PutAPIKey(ctx, APIKey{ID: id, Name: id})
			_, _ = s.GetAPIKey(ctx, id)
			_, _ = s.ListAPIKeys(ctx)
		}(i)
	}
	wg.Wait()
}
