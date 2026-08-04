package rank

import (
	"testing"
	"time"
)

// The example used to explain this scheme, checked against the implementation.
//
// Three hand-computed versions of it were wrong before anyone ran the code:
// twice on arithmetic, and once because the candidate billed as "the better
// match" was in a higher tier, so the gate settled the order before context
// ever applied. Any document describing the ranking should take its numbers
// from here.
func TestContextOutranksABetterMatchWithinTheSameTier(t *testing.T) {
	now := int64(1_800_000_000_000)
	hour := int64(time.Hour / time.Millisecond)
	day := 24 * hour

	frecency := func(ages ...int64) float64 {
		var c Counter
		for i := len(ages) - 1; i >= 0; i-- {
			c.Observe(now - ages[i])
		}
		return c.Value(now)
	}

	const query = "bld"

	// Used constantly, right here, but "bld" only matches it loosely.
	used, ok := MatchCommand("npm run build", Tokens(query))
	if !ok {
		t.Fatal("npm run build should match")
	}
	usedF := frecency(2*hour, 5*hour, 28*hour, 60*hour)
	usedCtx := Context{SameDir: true, SameHost: true, SameBranch: true}

	// Matches "bld" better — b and l are consecutive from position 0 — but was
	// run once, six months ago, somewhere else.
	stranger, ok := MatchCommand("blade-runner.sh --scan", Tokens(query))
	if !ok {
		t.Fatal("blade-runner.sh should match")
	}
	strangerF := frecency(180 * day)

	if used.Tier != stranger.Tier {
		t.Fatalf("the comparison only means something within one tier: %d vs %d",
			used.Tier, stranger.Tier)
	}
	if stranger.Score <= used.Score {
		t.Fatalf("the premise has gone: stranger %.3f should match better than %.3f",
			stranger.Score, used.Score)
	}

	usedScore := Score(used, usedF, usedCtx)
	strangerScore := Score(stranger, strangerF, Context{})
	if usedScore <= strangerScore {
		t.Errorf("context failed to outrank a better match: %.3f vs %.3f",
			usedScore, strangerScore)
	}

	t.Logf("query %q", query)
	t.Logf("  npm run build           tier %d  M %.3f  F %.2f  C %.3f  -> %.3f",
		used.Tier, used.Score, usedF, usedCtx.Multiplier(), usedScore)
	t.Logf("  blade-runner.sh --scan  tier %d  M %.3f  F %.2f  C %.3f  -> %.3f",
		stranger.Tier, stranger.Score, strangerF, Context{}.Multiplier(), strangerScore)
}
