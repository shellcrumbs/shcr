package theme

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ptyPair returns the two ends of a fresh pseudo-terminal: the side the program
// under test believes is its terminal, and the side a test can answer from.
func ptyPair(t *testing.T) (tty, peer *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx here: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock a pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pty: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pty slave: %v", err)
	}
	t.Cleanup(func() { slave.Close(); master.Close() })
	return slave, master
}

// answerable puts the environment in the state where the query is allowed to
// run at all.
func answerable(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	t.Setenv("TERM", "xterm-256color")
}

// The whole reason for not using termenv here. A terminal that never answers
// must cost the budget and no more — five seconds before the first frame is
// indistinguishable from a hang, and this runs on the Ctrl+R path.
func TestAnUnansweredQueryCostsOnlyTheBudget(t *testing.T) {
	answerable(t)
	tty, _ := ptyPair(t)

	started := time.Now()
	_, ok := queryDarkBackground(tty, 150*time.Millisecond)
	elapsed := time.Since(started)

	if ok {
		t.Error("a silent terminal reported an answer")
	}
	if elapsed > time.Second {
		t.Errorf("waited %v for a terminal that never answered", elapsed)
	}
}

func TestATerminalThatAnswersIsBelieved(t *testing.T) {
	answerable(t)
	for _, tc := range []struct {
		name  string
		reply string
		dark  bool
	}{
		{"a cream terminal", "\x1b]11;rgb:fdfd/f6f6/e3e3\x1b\\", false},
		{"a dark terminal", "\x1b]11;rgb:1e1e/1e1e/1e1e\a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tty, peer := ptyPair(t)
			go func() {
				buf := make([]byte, 64)
				if _, err := peer.Read(buf); err == nil {
					peer.WriteString(tc.reply)
				}
			}()

			dark, ok := queryDarkBackground(tty, 2*time.Second)
			if !ok {
				t.Fatal("the answer was not read")
			}
			if dark != tc.dark {
				t.Errorf("dark=%v, want %v", dark, tc.dark)
			}
		})
	}
}

// Nothing to ask, so nothing should be spent asking.
func TestNothingIsAskedOfSomethingThatIsNotATerminal(t *testing.T) {
	answerable(t)
	f, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	started := time.Now()
	if _, ok := queryDarkBackground(f, 2*time.Second); ok {
		t.Error("a regular file answered a terminal query")
	}
	if d := time.Since(started); d > 100*time.Millisecond {
		t.Errorf("spent %v on a file", d)
	}
}

// A multiplexer may swallow the query instead of forwarding it, and CI has no
// terminal at all. Both would pay the budget on every invocation for an answer
// that never comes.
func TestTheQueryIsSkippedWhereItCannotBeAnswered(t *testing.T) {
	tty, _ := ptyPair(t)
	for _, tc := range []struct{ env, val string }{
		{"TERM", "screen-256color"},
		{"TERM", "tmux-256color"},
		{"TERM", "dumb"},
		{"TERM", ""},
		{"CI", "true"},
	} {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			answerable(t)
			t.Setenv(tc.env, tc.val)
			started := time.Now()
			if _, ok := queryDarkBackground(tty, 2*time.Second); ok {
				t.Error("queried anyway")
			}
			if d := time.Since(started); d > 100*time.Millisecond {
				t.Errorf("spent %v before giving up", d)
			}
		})
	}
}
