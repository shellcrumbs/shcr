package rank

import (
	"math"
	"testing"
	"time"
)

func match(t *testing.T, command, query string) Match {
	t.Helper()
	m, ok := MatchCommand(command, Tokens(query))
	if !ok {
		t.Fatalf("%q did not match %q at all", query, command)
	}
	return m
}

func TestTierIsDecidedByWhereTheTokenLands(t *testing.T) {
	for _, tc := range []struct {
		command, query string
		want           Tier
	}{
		{"git push origin main", "git", TierPrefix},
		// The word-boundary rule: without it this is a fuzzy match, and
		// build-everything.sh outranks it on string position alone.
		{"npm run build", "build", TierPrefix},
		{"npm run build", "uild", TierSubstring},
		{"npm run build", "nrb", TierFuzzy},
		{"docker compose up -d", "up", TierPrefix},
		{"git commit -m fix", "gcm", TierFuzzy},
		{"make deploy", "zzz", TierNone},
	} {
		m, ok := MatchCommand(tc.command, Tokens(tc.query))
		if tc.want == TierNone {
			if ok {
				t.Errorf("%q should not match %q, got tier %d", tc.query, tc.command, m.Tier)
			}
			continue
		}
		if !ok || m.Tier != tc.want {
			t.Errorf("%q against %q: tier %d (matched %v), want %d",
				tc.query, tc.command, m.Tier, ok, tc.want)
		}
	}
}

// Query terms are matched independently, so their order in the query need not
// be their order in the command.
func TestTokensMatchOutOfOrder(t *testing.T) {
	if _, ok := MatchCommand("main-app.sh docker", Tokens("docker main")); !ok {
		t.Error("out-of-order terms should match")
	}
	if _, ok := MatchCommand("npm run build", Tokens("npm zzz")); ok {
		t.Error("a token that matches nothing must disqualify the candidate")
	}
	// The candidate is only as exact as its vaguest token.
	m := match(t, "npm run build", "npm bld")
	if m.Tier != TierFuzzy {
		t.Errorf("tier = %d, want the weakest token's tier (fuzzy)", m.Tier)
	}
}

// Typing a trailing space is something everyone does mid-thought. An empty
// token would match nothing and disqualify the entire history.
func TestWhitespaceInQueriesIsHarmless(t *testing.T) {
	for _, q := range []string{"npm", "npm ", " npm", "  npm  ", "npm\trun"} {
		if _, ok := MatchCommand("npm run build", Tokens(q)); !ok {
			t.Errorf("query %q matched nothing", q)
		}
	}
	if got := Tokens("   "); len(got) != 0 {
		t.Errorf("whitespace-only query produced tokens %q", got)
	}
	// An empty query matches everything rather than nothing: the picker opens
	// with no query and must still list history.
	if m, ok := MatchCommand("anything", Tokens("")); !ok || m.Tier != TierPrefix {
		t.Errorf("empty query: matched=%v tier=%d", ok, m.Tier)
	}
}

func TestTighterAndBetterPlacedMatchesScoreHigher(t *testing.T) {
	// Consecutive from the start beats the same characters scattered.
	tight := match(t, "mkdir -p build", "mkd")
	loose := match(t, "make deploy", "mkd")
	if tight.Score <= loose.Score {
		t.Errorf("tight %.3f should beat scattered %.3f", tight.Score, loose.Score)
	}
	// Characters on word starts beat characters mid-word.
	starts := match(t, "git commit -m", "gcm")
	middle := match(t, "gigacomputer", "gcm")
	if starts.Score <= middle.Score {
		t.Errorf("word-start match %.3f should beat mid-word %.3f", starts.Score, middle.Score)
	}
}

// A forward scan alone takes the first occurrence of every character, which can
// span the whole string when a tighter match sits further right. The command
// below has no contiguous "abc" anywhere — so this exercises the fuzzy path
// rather than falling through to a prefix match — but its last five characters
// hold a much tighter one than the forward scan finds first.
func TestFuzzyMatchTightensFromTheRight(t *testing.T) {
	//              0123456789
	m := match(t, "a__b__a-b-c", "abc")
	if m.Tier != TierFuzzy {
		t.Fatalf("tier = %d, want fuzzy — the test no longer exercises the fuzzy path", m.Tier)
	}
	// Tightened to a@6 b@8 c@10 (span 5) scores about 0.84; the untightened
	// a@0 b@3 c@10 (span 11) scores about 0.65.
	if m.Score < 0.75 {
		t.Errorf("score %.3f suggests the match was not tightened from the right", m.Score)
	}
}

func TestExitCodesAreClassifiedByMeaningNotMagnitude(t *testing.T) {
	for _, tc := range []struct {
		code int
		want Outcome
	}{
		{0, OutcomeSuccess},
		{1, OutcomeFailure},       // grep found nothing, or the build broke
		{2, OutcomeFailure},       //
		{126, OutcomeNeverRan},    // found, not executable
		{127, OutcomeNeverRan},    // not found at all
		{130, OutcomeInterrupted}, // Ctrl-C
		{137, OutcomeInterrupted}, // SIGKILL
		{143, OutcomeInterrupted}, // SIGTERM
	} {
		if got := ClassifyExit(tc.code); got != tc.want {
			t.Errorf("exit %d classified %d, want %d", tc.code, got, tc.want)
		}
	}
}

func TestContextCannotOutweighMatchQuality(t *testing.T) {
	// Every context signal at once, against none at all.
	best := Context{SameDir: true, SameSession: true, SameHost: true, SameBranch: true}
	if m := best.Multiplier(); m > maxContext {
		t.Errorf("multiplier %.3f exceeds the clamp %.2f", m, maxContext)
	}
	worst := Context{FailureRate: 1, NeverRan: true, AllImported: true}
	if m := worst.Multiplier(); m < minContext {
		t.Errorf("multiplier %.3f below the clamp %.2f", m, minContext)
	}
	// A tier gate is absolute: no amount of context promotes a fuzzy match over
	// a prefix one.
	prefix := Match{Tier: TierPrefix, Score: 0.5}
	fuzzy := Match{Tier: TierFuzzy, Score: 1.0}
	if !Less(prefix.Tier, Score(prefix, 0, Context{}), 0,
		fuzzy.Tier, Score(fuzzy, 1000, best), 0) {
		t.Error("a fuzzy match outranked a prefix match")
	}
}

// Mid-loop on a failing test, the failing command is what the user is reaching
// for — not the thing to hide.
func TestDebuggingLoopLiftsTheFailurePenalty(t *testing.T) {
	failing := Context{FailureRate: 1}
	debugging := Context{FailureRate: 1, Debugging: true}
	if debugging.Multiplier() <= failing.Multiplier() {
		t.Errorf("debugging %.3f should not be penalised like plain failure %.3f",
			debugging.Multiplier(), failing.Multiplier())
	}
	// A command that could not be found is still demoted, debugging or not.
	notFound := Context{NeverRan: true, Debugging: true}
	if notFound.Multiplier() >= 1 {
		t.Errorf("a command that never ran should be demoted, got %.3f", notFound.Multiplier())
	}
}

func TestFrecencyDecaysAndResistsBursts(t *testing.T) {
	const min = int64(60_000)
	var c Counter

	// Fifteen runs in two minutes, the way anyone stages a commit.
	base := int64(1_800_000_000_000)
	for i := range 15 {
		c.Observe(base + int64(i)*8_000)
	}
	burst := c.Value(base)
	if burst > 1.5 {
		t.Errorf("a two-minute burst counted as %.2f, want about 1", burst)
	}

	// The same command used once an hour for six hours is genuinely frequent.
	var spread Counter
	for i := range 6 {
		spread.Observe(base + int64(i)*60*min)
	}
	if v := spread.Value(base + 6*60*min); v < 4 {
		t.Errorf("six spread-out runs counted as %.2f, want most of 6", v)
	}

	// One half-life later, a single run is worth half.
	var one Counter
	one.Observe(base)
	if got, want := one.Value(base+HalfLife.Milliseconds()), 0.5; math.Abs(got-want) > 0.01 {
		t.Errorf("after one half-life: %.3f, want %.2f", got, want)
	}
}

// Sync delivers executions from machines whose clocks disagree. An arrival
// older than what we have already folded in must not inflate the weight.
func TestOutOfOrderExecutionsDoNotInflate(t *testing.T) {
	base := int64(1_800_000_000_000)
	day := int64(24 * time.Hour / time.Millisecond)

	var forward Counter
	forward.Observe(base - 2*day)
	forward.Observe(base)

	var backward Counter
	backward.Observe(base)
	backward.Observe(base - 2*day)

	f, b := forward.Value(base), backward.Value(base)
	if math.Abs(f-b) > 0.01 {
		t.Errorf("order changed the weight: forward %.3f, backward %.3f", f, b)
	}
	// And a late arrival must never make the weight exceed what two properly
	// ordered executions would give.
	if b > 2.01 {
		t.Errorf("weight %.3f exceeds two executions", b)
	}
}
