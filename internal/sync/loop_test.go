package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/crypto"
)

func cfg() LoopConfig {
	return LoopConfig{
		MinInterval: 30 * time.Second,
		MaxInterval: 3 * time.Hour,
		Jitter:      0, // determinism; jitter is exercised separately
		MaxBackoff:  15 * time.Minute,
	}.withDefaults()
}

// Nothing has run yet, so there is nothing to rate-limit.
func TestFirstSyncIsImmediate(t *testing.T) {
	if d := cfg().delay(time.Time{}, time.Time{}, "", 0); d != 0 {
		t.Fatalf("first sync delayed by %s", d)
	}
}

// The floor is the whole point: however many triggers arrive, two syncs are
// never closer together than MinInterval.
func TestTriggerInsideTheFloorIsDeferredNotDropped(t *testing.T) {
	c := cfg()
	now := time.Now()

	// A sync ran 5s ago and a trigger has arrived.
	d := c.delay(now.Add(-5*time.Second), now.Add(-5*time.Second), TriggerCommand, 0)
	want := 25 * time.Second
	if d < want-time.Second || d > want+time.Second {
		t.Fatalf("delay = %s, want about %s (the rest of the floor window)", d, want)
	}
	if d == 0 {
		t.Fatal("a trigger inside the window must not sync immediately")
	}
}

// Once the floor has passed, a trigger syncs at once.
func TestTriggerAfterTheFloorRunsImmediately(t *testing.T) {
	c := cfg()
	past := time.Now().Add(-31 * time.Second)
	if d := c.delay(past, past, TriggerPicker, 0); d != 0 {
		t.Fatalf("delay = %s, want 0", d)
	}
}

// With nothing happening the ceiling still fires, so an untouched machine
// converges.
func TestIdleFallsBackToTheCeiling(t *testing.T) {
	c := cfg()
	now := time.Now()
	d := c.delay(now, now, "", 0)
	if d < c.MaxInterval-time.Minute || d > c.MaxInterval+c.Jitter+time.Minute {
		t.Fatalf("idle delay = %s, want about %s", d, c.MaxInterval)
	}
	// And once the ceiling has elapsed, it goes now.
	old := now.Add(-4 * time.Hour)
	if d := c.delay(old, old, "", 0); d != 0 {
		t.Fatalf("overdue periodic delay = %s, want 0", d)
	}
}

// A backlog of failures must not silence the ceiling, and must not beat the
// floor either.
func TestBackoffNeverGoesBelowTheFloor(t *testing.T) {
	c := cfg()
	b := time.Duration(0)
	for i := range 8 {
		b = nextBackoff(b, c.MinInterval, c.MaxBackoff)
		if b < c.MinInterval {
			t.Fatalf("backoff %d is %s, below the %s floor", i, b, c.MinInterval)
		}
		if b > c.MaxBackoff {
			t.Fatalf("backoff %d is %s, above the %s cap", i, b, c.MaxBackoff)
		}
	}
	if b != c.MaxBackoff {
		t.Errorf("backoff settled at %s, want the cap %s", b, c.MaxBackoff)
	}

	// While backing off, a pending trigger does not shortcut the wait.
	now := time.Now()
	d := c.delay(now, now, TriggerCommand, 5*time.Minute)
	if d < 4*time.Minute {
		t.Errorf("a trigger cut the backoff short: %s", d)
	}
}

// The periodic deadline is measured from the last success, so a run of failures
// does not also stop the clock on the ceiling.
func TestCeilingIsMeasuredFromTheLastSuccess(t *testing.T) {
	c := cfg()
	now := time.Now()
	// Attempted a moment ago, but nothing has succeeded for four hours.
	d := c.delay(now.Add(-time.Minute), now.Add(-4*time.Hour), "", 0)
	if d != 0 {
		t.Fatalf("delay = %s; the ceiling should be overdue", d)
	}
}

// Triggers coalesce: several inside one window produce one sync, not several.
func TestTriggersCoalesce(t *testing.T) {
	e := &Engine{}
	e.EnableTriggers()
	for _, tr := range []Trigger{
		TriggerSessionStart, TriggerCommand, TriggerCommand, TriggerPicker,
	} {
		e.Trigger(tr)
	}
	if got := len(e.triggers); got != 1 {
		t.Fatalf("%d triggers queued, want 1 — they should collapse", got)
	}
	// The first one is what the log will name, which is the most specific
	// account of why this sync is happening.
	if got := <-e.triggers; got != TriggerSessionStart {
		t.Errorf("kept %q, want the first trigger", got)
	}
}

// Trigger must be safe to call from the daemon's ingest path, where blocking
// would stall recording a command.
func TestTriggerNeverBlocks(t *testing.T) {
	e := &Engine{}
	e.EnableTriggers()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			e.Trigger(TriggerCommand)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Trigger blocked")
	}
	// And on an engine with no loop running at all.
	(&Engine{}).Trigger(TriggerCommand)
}

func TestJitterStaysInRange(t *testing.T) {
	const j = 30 * time.Second
	for range 200 {
		got := jitterOf(j)
		if got < 0 || got >= j {
			t.Fatalf("jitter %s outside [0, %s)", got, j)
		}
	}
	if jitterOf(0) != 0 {
		t.Error("zero jitter should be zero")
	}
}

// A misconfiguration should not produce a ceiling below the floor.
func TestConfigDefaultsAreCoherent(t *testing.T) {
	c := LoopConfig{MinInterval: time.Hour, MaxInterval: time.Minute}.withDefaults()
	if c.MaxInterval < c.MinInterval {
		t.Errorf("ceiling %s is below the floor %s", c.MaxInterval, c.MinInterval)
	}
	d := LoopConfig{}.withDefaults()
	if d.MinInterval != 30*time.Second || d.MaxInterval != 3*time.Hour {
		t.Errorf("defaults drifted: %+v", d)
	}
}

// Sync being switched off must not need a daemon restart to take effect, and
// switching it back on must not either: the loop asks before every cycle.
func TestLoopFollowsTheEnabledSwitchWhileRunning(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)

	var mu sync.Mutex
	enabled := false
	a.engine.Enabled = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return enabled
	}
	a.run(t, "while switched off", 0, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.engine.Run(ctx, LoopConfig{
		MinInterval: time.Millisecond, MaxInterval: 20 * time.Millisecond,
		Jitter: 0, MaxBackoff: time.Millisecond,
	})

	// Nothing may be published while the switch is off.
	time.Sleep(80 * time.Millisecond)
	if keys, _ := bucket.List(ctx, batchPrefix("dev-a"), ""); len(keys) != 0 {
		t.Fatalf("pushed %d batches while sync was switched off", len(keys))
	}

	mu.Lock()
	enabled = true
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if keys, _ := bucket.List(ctx, batchPrefix("dev-a"), ""); len(keys) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("turning sync on did not start the loop syncing")
}
