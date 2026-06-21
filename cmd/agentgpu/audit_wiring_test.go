package main

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/config"
)

// TestAuditCheckpointPath proves the audit checkpoint lands alongside the other
// control-plane state files (next to the quota checkpoint).
func TestAuditCheckpointPath(t *testing.T) {
	got := auditCheckpointPath(filepath.Join("var", "lib", "agentgpu", "quota.json"))
	want := filepath.Join("var", "lib", "agentgpu", "audit.json")
	if got != want {
		t.Errorf("auditCheckpointPath = %q, want %q", got, want)
	}
}

// TestAuditReloadAtCmdBoundary proves AC4 at the cmd boundary: a store
// checkpointed to the derived path reloads its entries in a fresh store, so the
// trail survives a restart (a crash loses at most one checkpoint interval).
func TestAuditReloadAtCmdBoundary(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")
	path := auditCheckpointPath(quotaPath)

	s1 := audit.NewMemoryStore(auditLogCapacity)
	if err := s1.Append(audit.Entry{Actor: "admin1", Op: "key.create", Target: "k1", Outcome: audit.OutcomeSuccess}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s1.Checkpoint(path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Fresh store loads the checkpoint, mirroring server startup.
	s2 := audit.NewMemoryStore(auditLogCapacity)
	if err := s2.LoadCheckpoint(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := s2.List(audit.Filter{}, 0)
	if len(got) != 1 || got[0].Op != "key.create" || got[0].Actor != "admin1" {
		t.Fatalf("reloaded trail wrong: %+v", got)
	}
}

// TestLogHandleSetLevel proves the dynamic level seam (the #92 hook): SetLevel
// flips what the handler emits at runtime, with no restart.
func TestLogHandleSetLevel(t *testing.T) {
	h, err := newLoggerHandle(config.LogConfig{Level: "info", Format: "json", Output: filepath.Join(t.TempDir(), "log")})
	if err != nil {
		t.Fatalf("newLoggerHandle: %v", err)
	}
	defer func() { _ = h.Close() }()

	if h.Logger.Enabled(nil, slog.LevelDebug) {
		t.Fatal("debug should be disabled at the initial info level")
	}
	h.SetLevel(slog.LevelDebug)
	if !h.Logger.Enabled(nil, slog.LevelDebug) {
		t.Fatal("SetLevel(debug) did not enable debug logging")
	}
	// The ring is wired and live even though no endpoint reads it yet.
	if h.Ring == nil {
		t.Fatal("logHandle.Ring should be non-nil")
	}
	h.Logger.Info("hello")
	if h.Ring.Len() == 0 {
		t.Error("emitted record was not captured by the ring")
	}
}
