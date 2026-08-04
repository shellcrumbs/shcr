package rank

import (
	"math"
	"time"
)

// Tuning. These are starting guesses, and the only honest way to move them is
// against recorded acceptances — which query was typed, which candidate was
// taken, where it ranked — rather than against anyone's intuition.
const (
	// HalfLife is when an execution counts half as much as one now.
	HalfLife = 72 * time.Hour
	// Beta is how far frecency can lift a match.
	Beta = 0.6
	// BurstWindow suppresses repeats: running `git status` fifteen times while
	// staging is one use of it, not fifteen.
	BurstWindow = 5 * time.Minute
	// DebugWindow is how recently a failure counts as an active debugging loop.
	DebugWindow = 15 * time.Minute

	// Context is clamped so that several mild signals cannot combine into a
	// veto over match quality.
	minContext = 0.6
	maxContext = 2.0
)

// Outcome is what an exit code actually tells you, which is not what its
// magnitude suggests.
type Outcome int

const (
	// OutcomeSuccess: exit 0.
	OutcomeSuccess Outcome = iota
	// OutcomeFailure: it ran and reported a problem. Ambiguous on its own —
	// grep, diff and test all exit 1 in ordinary use — so it is worth little.
	OutcomeFailure
	// OutcomeNeverRan: 126 and 127, not executable and not found. The one
	// unambiguous signal in the set: re-running a mistyped command name is
	// never what anyone wants.
	OutcomeNeverRan
	// OutcomeInterrupted: killed by a signal. 130 is Ctrl-C, easily the most
	// common non-zero exit at an interactive prompt, and it says nothing at all
	// about the command. Any rule keyed on "exit code > 127" penalises mostly
	// this.
	OutcomeInterrupted
)

// ClassifyExit sorts an exit code by what it means rather than how large it is.
func ClassifyExit(code int) Outcome {
	switch {
	case code == 0:
		return OutcomeSuccess
	case code == 126, code == 127:
		return OutcomeNeverRan
	case code > 128 && code <= 165:
		return OutcomeInterrupted
	}
	return OutcomeFailure
}

// Context is how a candidate relates to where the user is standing. Each field
// asks whether *any* execution of the command matched, not whether the most
// recent one did: the command you ran in this directory last week should still
// win here over one you ran elsewhere yesterday.
type Context struct {
	SameDir     bool
	SameRepo    bool
	SameSession bool
	SameHost    bool
	SameBranch  bool

	// FailureRate covers only executions that ran and finished, so interrupted,
	// orphaned and still-running ones neither help nor hurt.
	FailureRate float64
	// NeverRan is set when the command could not be found or executed.
	NeverRan bool
	// Debugging is set when the most recent execution failed within
	// DebugWindow. Mid-loop on a failing test, that command is precisely what
	// the user is reaching for, so the failure penalty is lifted rather than
	// applied hardest exactly when it is least wanted.
	Debugging bool
	// AllImported marks a command known only from an imported history file: no
	// exit codes, approximate times, half its signals missing rather than bad.
	AllImported bool
}

// Multiplier is the combined context weight, clamped.
func (c Context) Multiplier() float64 {
	m := 1.0
	switch {
	case c.SameDir:
		m *= 1.35
	case c.SameRepo:
		m *= 1.20
	}
	if c.SameSession {
		m *= 1.15
	}
	if c.SameHost {
		m *= 1.10
	}
	if c.SameBranch {
		m *= 1.10
	}
	switch {
	case c.NeverRan:
		m *= 0.6
	case c.Debugging:
		// left at 1.0 on purpose
	case c.FailureRate > 0:
		m *= 1 - 0.25*c.FailureRate
	}
	if c.AllImported {
		m *= 0.90
	}
	return math.Min(math.Max(m, minContext), maxContext)
}

// Score combines a match with how often the command is used and how well it
// fits the current context.
//
// Multiplicative on the match score, so context scales a match rather than
// rescuing a bad one; logarithmic on frecency, so a command run in a loop two
// hundred times does not own the list.
func Score(m Match, frecency float64, c Context) float64 {
	if m.Tier == TierNone {
		return 0
	}
	return m.Score * (1 + Beta*math.Log1p(frecency)) * c.Multiplier()
}

// Less orders two scored candidates. Tier first — the hard gate — then score,
// then the more recent execution, so that equal candidates hold still instead
// of swapping places between keystrokes.
func Less(aTier Tier, aScore float64, aLast int64, bTier Tier, bScore float64, bLast int64) bool {
	if aTier != bTier {
		return aTier > bTier
	}
	if aScore != bScore {
		return aScore > bScore
	}
	return aLast > bLast
}
