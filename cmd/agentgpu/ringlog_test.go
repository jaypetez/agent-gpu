package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestLogRingDropsOldest proves the ring is a bounded, drop-oldest window: after
// appending more records than its capacity, only the most recent capacity
// records survive, oldest-first.
func TestLogRingDropsOldest(t *testing.T) {
	r := newLogRing(3)
	for i := 0; i < 5; i++ {
		r.append(logRecord{Message: string(rune('a' + i))})
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	snap := r.Snapshot()
	if len(snap) != 3 || snap[0].Message != "c" || snap[2].Message != "e" {
		t.Fatalf("snapshot window wrong: %v", messages(snap))
	}
}

// TestLogRingPartial proves a not-yet-full ring snapshots only what it holds.
func TestLogRingPartial(t *testing.T) {
	r := newLogRing(5)
	r.append(logRecord{Message: "x"})
	r.append(logRecord{Message: "y"})
	snap := r.Snapshot()
	if len(snap) != 2 || snap[0].Message != "x" || snap[1].Message != "y" {
		t.Fatalf("partial snapshot wrong: %v", messages(snap))
	}
}

// TestLogRingDisabled proves a non-positive capacity disables buffering.
func TestLogRingDisabled(t *testing.T) {
	r := newLogRing(0)
	r.append(logRecord{Message: "x"})
	if r.Len() != 0 || r.Snapshot() != nil {
		t.Fatalf("disabled ring should hold nothing: len=%d snap=%v", r.Len(), r.Snapshot())
	}
}

// TestRingHandlerCapturesRecords proves a logger built over the ring handler both
// forwards to the terminal handler and appends each record to the ring with its
// message, level, and attributes.
func TestRingHandlerCapturesRecords(t *testing.T) {
	ring := newLogRing(10)
	terminal := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(newRingHandler(terminal, ring))

	logger.Info("hello", "model", "llama3", "n", 3)
	logger.Warn("careful", "code", "x")

	snap := ring.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("ring captured %d records, want 2", len(snap))
	}
	if snap[0].Message != "hello" || snap[0].Level != slog.LevelInfo {
		t.Errorf("record 0 = %+v", snap[0])
	}
	if snap[0].Attrs["model"] != "llama3" {
		t.Errorf("record 0 attrs missing model: %+v", snap[0].Attrs)
	}
	if snap[1].Message != "careful" || snap[1].Level != slog.LevelWarn {
		t.Errorf("record 1 = %+v", snap[1])
	}
}

// TestRingHandlerRespectsLevel proves a record below the enabled level is neither
// emitted nor rung (the dynamic level governs both).
func TestRingHandlerRespectsLevel(t *testing.T) {
	ring := newLogRing(10)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	terminal := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: lv})
	logger := slog.New(newRingHandler(terminal, ring))

	logger.Debug("filtered out") // below info
	logger.Info("kept")

	snap := ring.Snapshot()
	if len(snap) != 1 || snap[0].Message != "kept" {
		t.Fatalf("level filtering wrong; ring = %v", messages(snap))
	}

	// Lower the dynamic level: now debug is captured too.
	lv.Set(slog.LevelDebug)
	logger.Debug("now kept")
	snap = ring.Snapshot()
	if len(snap) != 2 || snap[1].Message != "now kept" {
		t.Fatalf("dynamic level change not honored; ring = %v", messages(snap))
	}
}

// TestRingHandlerRedactsSecrets proves the ring NEVER holds secret values: a
// record logged with a sensitive key (directly or inside a group) is redacted in
// the captured copy, matching the terminal handler's ReplaceAttr policy.
func TestRingHandlerRedactsSecrets(t *testing.T) {
	ring := newLogRing(10)
	terminal := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{ReplaceAttr: redactAttr})
	logger := slog.New(newRingHandler(terminal, ring))

	logger.Info("auth", "token", "supersecret", "model", "llama3")
	logger.Info("nested", slog.Group("creds", slog.String("secret", "topsecret")))
	// Attrs bound via With must also be redacted in the ring.
	logger.With("api_key", "leakme").Info("withattr")

	snap := ring.Snapshot()
	for _, rec := range snap {
		for k, v := range rec.Attrs {
			s, _ := v.(string)
			if s == "supersecret" || s == "topsecret" || s == "leakme" {
				t.Fatalf("ring leaked a secret under %q: %v", k, v)
			}
		}
	}
	// Sanity: the redacted placeholder is present and the non-secret survives.
	if snap[0].Attrs["token"] != redactedPlaceholder {
		t.Errorf("token not redacted in ring: %v", snap[0].Attrs["token"])
	}
	if snap[0].Attrs["model"] != "llama3" {
		t.Errorf("non-secret attr lost: %v", snap[0].Attrs["model"])
	}
	if snap[1].Attrs["creds.secret"] != redactedPlaceholder {
		t.Errorf("grouped secret not redacted: %v", snap[1].Attrs)
	}
	if snap[2].Attrs["api_key"] != redactedPlaceholder {
		t.Errorf("With() secret not redacted: %v", snap[2].Attrs)
	}
}

// TestRingHandlerEnabled proves Enabled defers to the wrapped handler.
func TestRingHandlerEnabled(t *testing.T) {
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	h := newRingHandler(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: lv}), newLogRing(1))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info should be disabled at warn level")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error should be enabled at warn level")
	}
}

func messages(recs []logRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Message
	}
	return out
}
