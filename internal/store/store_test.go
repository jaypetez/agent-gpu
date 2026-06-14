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
	if got != key {
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
