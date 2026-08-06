package rank

import "testing"

// A camelCase hump starts a word as surely as a dash or a slash does. Without
// it, searching `build` finds npmRunBuild only as a mid-word substring, so it
// loses to anything that happens to have the letters at a boundary — the exact
// prefix blindness TierPrefix exists to prevent.
func TestCamelCaseHumpsStartWords(t *testing.T) {
	for _, tc := range []struct {
		command string
		token   string
	}{
		{"npmRunBuild", "build"},
		{"gradlew assembleRelease", "release"},
		{"./myScript.sh", "script"},
		{"docker buildXCache", "cache"},
	} {
		m, ok := MatchCommand(tc.command, Tokens(tc.token))
		if !ok {
			t.Fatalf("%q did not match %q at all", tc.command, tc.token)
		}
		if m.Tier != TierPrefix {
			t.Errorf("%q vs %q: tier %v, want TierPrefix — the hump is a word start",
				tc.command, tc.token, m.Tier)
		}
	}
}

// Case is ignored for matching, which must stay true now that boundaries are
// found on the original text.
func TestMatchingStaysCaseInsensitive(t *testing.T) {
	for _, tc := range []struct{ command, token string }{
		{"NPM run BUILD", "npm"},
		{"npm run build", "NPM"},
		{"Docker Compose Up", "compose"},
	} {
		if _, ok := MatchCommand(tc.command, Tokens(tc.token)); !ok {
			t.Errorf("%q did not match %q", tc.command, tc.token)
		}
	}
}

// Lowercasing can change how many runes a string has. Boundaries are indexed
// against the matched text, so the two must stay the same length or the indexes
// address the wrong characters.
func TestBoundariesLineUpWithOddCaseFolding(t *testing.T) {
	for _, cmd := range []string{
		"İstanbul deploy", // U+0130, whose lowercase is two runes
		"KELVIN K thing",  // U+212A KELVIN SIGN folds to "k"
		"straße build",    // ß
	} {
		if _, ok := MatchCommand(cmd, Tokens("deploy")); !ok && cmd == "İstanbul deploy" {
			t.Errorf("%q lost a token to case folding", cmd)
		}
		// The real assertion is that it does not panic on a bad index.
		_, _ = MatchCommand(cmd, Tokens("thing"))
	}
}

// The capital that starts a word is the last one in a run, not the first.
func TestAcronymRunsEndWhereTheWordBegins(t *testing.T) {
	for _, tc := range []struct{ command, token string }{
		{"parseHTTPResponse", "response"},
		{"awsS3Sync", "sync"},
		{"buildXCache", "cache"},
	} {
		m, ok := MatchCommand(tc.command, Tokens(tc.token))
		if !ok || m.Tier != TierPrefix {
			t.Errorf("%q vs %q: tier %v ok=%v, want TierPrefix", tc.command, tc.token, m.Tier, ok)
		}
	}
	// And the middle of a run is not a word start, or every capital would be.
	if m, _ := MatchCommand("parseHTTPResponse", Tokens("ttp")); m.Tier == TierPrefix {
		t.Error("the middle of an acronym counted as a word start")
	}
}
