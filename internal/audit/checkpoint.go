package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Permission bits for the checkpoint file and its parent directory (owner-only),
// matching the rest of the control-plane state. The audit log holds no secrets
// (entries are redacted), but defense in depth is cheap and keeps the on-disk
// surface uniform with the quota/session checkpoints.
const (
	checkpointMode    os.FileMode = 0o600
	checkpointDirMode os.FileMode = 0o700
)

// Checkpoint atomically writes the current entries to path (write temp +
// rename), creating the parent directory if needed. It is the durability seam:
// the server checkpoints periodically and on graceful shutdown, so a crash loses
// at most one checkpoint interval of audit entries. The append-only log is
// written whole each time (the entry counts a control plane produces are modest,
// matching the quota/session stores' whole-snapshot approach).
func (m *MemoryStore) Checkpoint(path string) error {
	if path == "" {
		return fmt.Errorf("audit: empty checkpoint path")
	}

	m.mu.RLock()
	snapshot := make([]Entry, len(m.entries))
	for i, e := range m.entries {
		snapshot[i] = e.clone()
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("audit: marshal checkpoint: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, checkpointDirMode); err != nil {
		return fmt.Errorf("audit: create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".audit-*.tmp")
	if err != nil {
		return fmt.Errorf("audit: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(checkpointMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("audit: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("audit: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("audit: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("audit: rename checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint replaces the store's entries with those persisted at path. A
// missing file is not an error (a fresh store). The cap is re-applied on load
// (the newest maxEntries are kept) so a checkpoint written before the cap was
// tightened does not exceed it after restart.
func (m *MemoryStore) LoadCheckpoint(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("audit: read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var loaded []Entry
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("audit: parse checkpoint %s: %w", path, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.max > 0 && len(loaded) > m.max {
		loaded = loaded[len(loaded)-m.max:]
	}
	m.entries = loaded
	return nil
}
