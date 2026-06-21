package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Persistence for the runtime config overrides (#92). Only the fields changed via
// PUT are checkpointed (an override map keyed by field name), NOT the whole config
// — so a fresh boot resolves config normally (flag > env > default) and then only
// the operator's explicit PUT changes are re-applied on top, surviving the
// restart. The on-disk format and the atomic write discipline mirror the quota
// counter checkpoint (internal/quota/store.go): write a temp file with owner-only
// perms, then rename it into place; a missing file is not an error.

// configCheckpointMode / configCheckpointDirMode are the owner-only permission
// bits for the checkpoint file and its parent directory, matching the quota/audit
// checkpoints. These overrides are not secrets, but defense in depth is cheap and
// keeps the state directory uniformly locked down.
const (
	configCheckpointMode    os.FileMode = 0o600
	configCheckpointDirMode os.FileMode = 0o700
)

// checkpoint atomically writes the current override set to checkpointPath (temp +
// rename), creating the parent directory if needed. The override set is the value
// of each PUT-changed field, projected from the current config — so the file
// always reflects the operator's cumulative changes. An empty checkpointPath
// disables persistence (a no-op success), so unit tests need not touch the
// filesystem. Callers must not hold mu (checkpoint takes the read lock to snapshot).
func (rc *runtimeConfig) checkpoint() error {
	if rc.checkpointPath == "" {
		return nil
	}

	rc.mu.RLock()
	overrides := make(map[string]any, len(rc.persisted))
	for f := range rc.persisted {
		overrides[f] = fieldValue(rc.cur, f)
	}
	rc.mu.RUnlock()

	data, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal checkpoint: %w", err)
	}

	dir := filepath.Dir(rc.checkpointPath)
	if err := os.MkdirAll(dir, configCheckpointDirMode); err != nil {
		return fmt.Errorf("config: create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(configCheckpointMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmpName, rc.checkpointPath); err != nil {
		return fmt.Errorf("config: rename checkpoint: %w", err)
	}
	return nil
}

// LoadConfigCheckpoint reads the persisted override set from path and re-applies
// each override to the live subsystems via s's runtime-config holder, exactly as a
// PUT would (#92) — so an operator's prior config changes survive a restart and
// win over the boot flags for those tunable fields. A missing or empty file is not
// an error (a fresh boot with no overrides). Unknown/boot-only keys and invalid
// values in a hand-edited file are skipped with no fatal error (the boot value
// stands) so a corrupted checkpoint cannot wedge startup, mirroring the
// silent-fallback discipline of the config resolvers. It is a no-op when no
// runtime config is wired. It is called once at boot, after the subsystems are
// constructed and WithRuntimeConfig has seeded the holder with the resolved boot
// values.
func (s *Server) LoadConfigCheckpoint(path string) error {
	if s.config == nil || path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config: parse checkpoint %s: %w", path, err)
	}

	// Drop any boot-only or unknown key a hand-edited file may carry, so the
	// re-apply only touches genuine tunable fields.
	for _, key := range sortedKeys(raw) {
		if isReadOnlyField(key) || !isTunableField(key) {
			delete(raw, key)
			s.log.Warn("config checkpoint: skipping non-tunable override", "field", key)
		}
	}
	if len(raw) == 0 {
		return nil
	}

	// Serialize against any (in practice none, at boot) concurrent PUT, sharing the
	// PUT's read-modify-apply-commit critical section so the re-apply is atomic.
	s.config.writeMu.Lock()
	defer s.config.writeMu.Unlock()

	s.config.mu.RLock()
	pending := s.config.cur
	s.config.mu.RUnlock()

	changed, err := applyConfigPatch(&pending, raw)
	if err != nil {
		// A bad value in the persisted file is not fatal: log and keep the boot
		// values rather than wedging startup.
		s.log.Warn("config checkpoint: skipping invalid override; keeping boot value", "err", err)
		return nil
	}
	if err := s.config.apply(pending, changed); err != nil {
		s.log.Warn("config checkpoint: applier rejected persisted override; keeping boot value", "err", err)
		return nil
	}
	s.log.Info("config checkpoint loaded; re-applied persisted overrides", "fields", sortedKeys(raw))
	return nil
}
