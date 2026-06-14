package worker

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/types"
)

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 100 * time.Millisecond, Max: 2 * time.Second, Factor: 2.0}
	// No jitter (nil rng) so we can assert the deterministic ceiling per attempt.
	prev := time.Duration(0)
	for attempt := 0; attempt < 10; attempt++ {
		d := b.Delay(attempt, nil)
		if d > b.Max {
			t.Fatalf("attempt %d: delay %v exceeds Max %v", attempt, d, b.Max)
		}
		// Monotonic non-decreasing until the cap.
		if d < prev && prev < b.Max {
			t.Fatalf("attempt %d: delay %v dropped below previous %v before cap", attempt, d, prev)
		}
		prev = d
	}
	// High attempt count must be capped, not overflow.
	if got := b.Delay(1000, nil); got != b.Max {
		t.Fatalf("very large attempt should cap at Max, got %v", got)
	}
}

func TestBackoffJitterWithinBounds(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 100 * time.Millisecond, Max: 1 * time.Second, Factor: 2.0}
	rng := rand.New(rand.NewSource(1))
	for attempt := 0; attempt < 100; attempt++ {
		d := b.Delay(attempt, rng)
		if d < 0 || d > b.Max {
			t.Fatalf("attempt %d: jittered delay %v out of [0, Max]", attempt, d)
		}
	}
}

func TestBackoffZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()
	var b Backoff // zero value
	d := b.Delay(0, nil)
	if d <= 0 {
		t.Fatalf("zero-value backoff should still produce a positive base delay, got %v", d)
	}
}

func TestEchoExecutor(t *testing.T) {
	t.Parallel()
	res := EchoExecutor{}.Execute(context.Background(), types.Job{ID: "j1", Prompt: "hi"})
	if res.JobID != "j1" || res.Output != "echo: hi" || res.Err != nil {
		t.Fatalf("unexpected echo result: %+v", res)
	}
}
