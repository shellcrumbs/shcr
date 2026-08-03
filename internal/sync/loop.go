package sync

import (
	"context"
	"math/rand/v2"
	"time"
)

// Trigger is something that happened which makes syncing worthwhile now.
//
// Triggers exist so the cadence does not have to be guessed from a polling
// interval. Polling frequently enough to feel fresh means waking a sleeping
// laptop all day; polling rarely enough to be cheap means sitting down to stale
// history. Reacting to the moments that actually matter gives both.
type Trigger string

const (
	// TriggerDaemonStart fires once, when the daemon comes up.
	TriggerDaemonStart Trigger = "daemon-start"
	// TriggerCommand fires when something is recorded locally.
	TriggerCommand Trigger = "command"
	// TriggerSessionStart fires when a shell loads the hooks — you have just
	// sat down, and this is the moment peer history is most worth having.
	TriggerSessionStart Trigger = "session-start"
	// TriggerSessionEnd fires when a shell exits, pushing what you just did
	// before the machine is closed.
	TriggerSessionEnd Trigger = "session-end"
	// TriggerPicker fires when Ctrl+R opens: you are about to search history,
	// and it says you are actively working.
	TriggerPicker Trigger = "picker"
	// TriggerManual is `shcr sync now` or the dashboard button.
	TriggerManual Trigger = "manual"
)

// LoopConfig is a floor, a ceiling, and nothing else to tune.
type LoopConfig struct {
	// MinInterval is the floor: two syncs are never closer together than this,
	// however many triggers arrive. Triggers inside the window are coalesced
	// into one sync at the end of it.
	MinInterval time.Duration
	// MaxInterval is the ceiling: a sync happens at least this often even if
	// nothing at all triggers one, so a machine nobody touches still converges.
	MaxInterval time.Duration
	// Jitter spreads the periodic deadline so machines booted together do not
	// poll in lockstep. It is not applied to triggered syncs, which should feel
	// immediate.
	Jitter time.Duration
	// MaxBackoff caps the retry delay after failures.
	MaxBackoff time.Duration
}

func DefaultLoopConfig() LoopConfig {
	return LoopConfig{
		MinInterval: 30 * time.Second,
		MaxInterval: 3 * time.Hour,
		Jitter:      30 * time.Second,
		MaxBackoff:  15 * time.Minute,
	}
}

func (c LoopConfig) withDefaults() LoopConfig {
	d := DefaultLoopConfig()
	if c.MinInterval <= 0 {
		c.MinInterval = d.MinInterval
	}
	if c.MaxInterval <= 0 {
		c.MaxInterval = d.MaxInterval
	}
	if c.MaxInterval < c.MinInterval {
		c.MaxInterval = c.MinInterval
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = d.MaxBackoff
	}
	return c
}

// Trigger asks for a sync. It never blocks and never syncs inline, so it is
// safe to call from the daemon's ingest path or a shell hook.
//
// A trigger arriving inside the floor window is not dropped — it is remembered,
// and the sync happens the moment the window closes. Dropping it would mean the
// last command before you close a laptop is the one that does not get pushed.
func (e *Engine) Trigger(t Trigger) {
	if e.triggers == nil {
		return
	}
	select {
	case e.triggers <- t:
	default:
		// A trigger is already queued; one sync will cover both.
	}
}

// EnableTriggers prepares the trigger channel. Call before Run.
func (e *Engine) EnableTriggers() {
	if e.triggers == nil {
		e.triggers = make(chan Trigger, 1)
	}
}

// Run drives sync until the context is cancelled.
func (e *Engine) Run(ctx context.Context, cfg LoopConfig) error {
	cfg = cfg.withDefaults()
	e.EnableTriggers()

	var (
		// lastAttempt governs the floor: failures must not be allowed to
		// hammer the backend any more than successes.
		lastAttempt time.Time
		// lastSuccess governs the ceiling, so a run of failures does not also
		// silence the periodic sync.
		lastSuccess time.Time
		pending     Trigger
		backoff     time.Duration
	)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		resetTimer(timer, cfg.delay(lastAttempt, lastSuccess, pending, backoff))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-e.triggers:
			if pending == "" {
				pending = t
			}
			// Recompute the deadline with the trigger in hand.
			continue
		case <-timer.C:
		}

		reason := pending
		if reason == "" {
			reason = "periodic"
		}
		pending = ""
		lastAttempt = time.Now()

		// Checked here rather than before the loop starts, so turning sync off
		// and on again — from the dashboard, or by editing the config — takes
		// effect on a running daemon instead of at the next restart.
		if e.Enabled != nil && !e.Enabled() {
			continue
		}

		pushed, pulled, err := e.SyncOnce(ctx)
		if err != nil {
			backoff = nextBackoff(backoff, cfg.MinInterval, cfg.MaxBackoff)
			e.logf("sync (%s) failed, retrying in %s: %v", reason, backoff.Round(time.Second), err)
			continue
		}
		backoff = 0
		lastSuccess = time.Now()
		if pushed > 0 || pulled > 0 {
			e.logf("sync (%s): pushed %d, pulled %d", reason, pushed, pulled)
		}
	}
}

// delay is when the next sync may run.
func (c LoopConfig) delay(lastAttempt, lastSuccess time.Time, pending Trigger, backoff time.Duration) time.Duration {
	now := time.Now()

	// Nothing has run yet: go immediately. The daemon's own start is a trigger,
	// and waiting out a ceiling before the first sync would be absurd.
	if lastAttempt.IsZero() {
		return 0
	}

	// The floor applies to every attempt, including retries. Backoff only ever
	// pushes the next attempt further out, never closer.
	wait := c.MinInterval
	if backoff > wait {
		wait = backoff
	}
	floor := lastAttempt.Add(wait)

	if pending != "" || backoff > 0 {
		return until(now, floor)
	}

	// Idle: the ceiling, jittered, but never inside the floor.
	periodic := lastSuccess.Add(c.MaxInterval + jitterOf(c.Jitter))
	if periodic.Before(floor) {
		periodic = floor
	}
	return until(now, periodic)
}

func until(now, deadline time.Time) time.Duration {
	if d := deadline.Sub(now); d > 0 {
		return d
	}
	return 0
}

// jitterOf returns a spread in [0, jitter). Applied only to the periodic
// deadline: a triggered sync should feel immediate, and the floor already stops
// triggers from becoming a stampede.
func jitterOf(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(jitter)))
}

// nextBackoff doubles, starting from the floor — retrying faster than the floor
// allows would defeat the rate limit at exactly the moment something is wrong.
func nextBackoff(current, floor, max time.Duration) time.Duration {
	if current <= 0 {
		return floor
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
