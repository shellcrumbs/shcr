package rank

import "math"

// Counter is an exponentially decayed count of how often a command is used,
// held per distinct command text.
//
// It is deliberately *not* a sum over stored executions. Recomputing that on
// every keystroke means walking the history each time; this holds one number
// and one timestamp, costs one multiply per execution and one exponential per
// query, and never overflows — which the obvious alternative, accumulating
// e^(+λt) so that it can be decayed later, does spectacularly with millisecond
// timestamps.
//
// It is a local ranking cache, not history. The value depends on the order
// executions were observed in, so two machines that sync the same commands in
// different orders will hold slightly different weights. That is fine, and the
// alternative is not: making it order-independent means recomputing from the
// full execution set, which is the work this exists to avoid. Rebuild it at
// daemon start and the divergence never accumulates.
type Counter struct {
	// Weight is the decayed count as of At.
	Weight float64
	// At is when Weight was last brought up to date.
	At int64
	// LastCounted is the last execution that was actually counted, as opposed
	// to suppressed as part of a burst.
	LastCounted int64
}

// lambda is the decay rate implied by HalfLife, per millisecond.
func lambda() float64 {
	return math.Ln2 / float64(HalfLife.Milliseconds())
}

// Observe records one execution at time t, in unix milliseconds.
//
// Call it only for executions that were new — replaying an event that was
// already counted would inflate the weight, and shcr's store will happily hand
// the same event over twice.
func (c *Counter) Observe(t int64) {
	l := lambda()

	age := 0.0
	if d := t - c.At; d >= 0 {
		c.Weight *= math.Exp(-l * float64(d))
		c.At = t
	} else {
		// The execution is older than what we have already folded in: it
		// arrived late from another machine, or a clock disagreed. Counting it
		// at full weight would make a stale command look fresh, and rewinding
		// At would inflate everything counted since. Fold it in at its own age
		// instead and leave the clock alone.
		age = float64(-d)
	}

	// One count per burst window. Measured from the last *counted* execution,
	// not the last seen one — otherwise a command run every four minutes all
	// day would be counted exactly once. Absolute difference, so a late arrival
	// landing inside an existing burst is suppressed too.
	if abs64(t-c.LastCounted) <= BurstWindow.Milliseconds() && c.LastCounted != 0 {
		return
	}
	c.Weight += math.Exp(-l * age)
	if t > c.LastCounted {
		c.LastCounted = t
	}
}

// Value is the weight as of now, without modifying the counter.
func (c *Counter) Value(now int64) float64 {
	if d := now - c.At; d > 0 {
		return c.Weight * math.Exp(-lambda()*float64(d))
	}
	return c.Weight
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
