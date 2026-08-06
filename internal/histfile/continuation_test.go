package histfile

import (
	"strings"
	"testing"
	"time"
)

// A trailing backslash continues a line only when it is not itself escaped.
// `echo C:\\` ends in a literal backslash and is a whole command; reading it as
// a continuation swallows whatever the user ran next and stores the two as one.
func TestAnEscapedTrailingBackslashDoesNotContinue(t *testing.T) {
	for _, shell := range []Kind{Bash, Zsh} {
		t.Run(string(shell), func(t *testing.T) {
			raw := "echo C:\\\\\nls -al\ngit status\n"
			if shell == Zsh {
				raw = ": 1700000000:0;echo C:\\\\\n: 1700000001:0;ls -al\n: 1700000002:0;git status\n"
			}
			src := parseFor(t, shell, raw)
			var got []string
			for _, e := range src {
				got = append(got, e.Command)
			}
			if len(got) != 3 {
				t.Fatalf("got %d entries, want 3: %q", len(got), got)
			}
			if !strings.HasSuffix(got[0], `\\`) && !strings.HasSuffix(got[0], `\`) {
				t.Errorf("first entry lost its backslash: %q", got[0])
			}
			if strings.Contains(got[0], "ls -al") {
				t.Errorf("the next command was swallowed into the first: %q", got[0])
			}
		})
	}
}

// An odd number of trailing backslashes really is a continuation: the last one
// escapes the newline.
func TestAnUnescapedTrailingBackslashStillContinues(t *testing.T) {
	src := parseFor(t, Bash, "echo one \\\nand two\nls\n")
	if len(src) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(src), src)
	}
	if !strings.Contains(src[0].Command, "and two") {
		t.Errorf("the continuation was not joined: %q", src[0].Command)
	}
}

func parseFor(t *testing.T, kind Kind, raw string) []Entry {
	t.Helper()
	mod := time.UnixMilli(1_700_000_100_000)
	switch kind {
	case Zsh:
		return parseZsh([]byte(raw), mod)
	default:
		return parseBash([]byte(raw), mod)
	}
}
