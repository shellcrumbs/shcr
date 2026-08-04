package tui

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plainText(s string) string { return ansiRe.ReplaceAllString(s, "") }

// squash drops all whitespace, because the pane wraps on character boundaries:
// a command's tail can be split mid-token across two lines and still be fully
// present.
func squash(s string) string { return strings.Join(strings.Fields(plainText(s)), "") }

func testModel(w, h int) *Model {
	m := New(nil, theme.NewWithMode(io.Discard, theme.ColorNever), "")
	m.width, m.height = w, h
	m.localHost = "laptop"
	return m
}

func TestLayoutDropsDetailPaneWhenNarrow(t *testing.T) {
	wide := testModel(minSplitWidth, 30)
	if !wide.split() {
		t.Error("should split at the threshold")
	}
	narrow := testModel(minSplitWidth-1, 30)
	if narrow.split() {
		t.Error("should not split below the threshold")
	}
	l, r := narrow.paneWidths()
	if r != 0 || l != narrow.width-2 {
		t.Errorf("narrow layout should give the list everything, got left=%d right=%d", l, r)
	}
	l, r = wide.paneWidths()
	if l+r+1 != wide.width-2 {
		t.Errorf("panes plus divider should fill the frame: %d + %d + 1 != %d", l, r, wide.width-2)
	}
}

// One line per command is the whole point of the row rework: a normal terminal
// should show twenty-odd commands, not a handful.
func TestOneRowPerCommand(t *testing.T) {
	m := testModel(120, 30)
	if got, want := m.visibleItems(), m.bodyHeight(); got != want {
		t.Fatalf("visibleItems = %d, want %d (one row per command)", got, want)
	}
	if m.visibleItems() < 20 {
		t.Errorf("a 30-line terminal should show at least 20 commands, shows %d", m.visibleItems())
	}
	if testModel(120, 14).visibleItems() < 1 {
		t.Error("there must always be room for at least one item")
	}
}

func TestEveryRenderedLineIsExactlyTheFrameWidth(t *testing.T) {
	for _, size := range [][2]int{{120, 30}, {100, 24}, {80, 20}, {70, 12}, {40, 10}} {
		w, h := size[0], size[1]
		m := testModel(w, h)
		m.results = []store.Command{
			mkCmd("npm run build:prod", store.StatusFailed, 127, 2400, "build-server"),
			mkCmd("git status --short", store.StatusCompleted, 0, 12, "laptop"),
			mkCmd("sleep 30 &", store.StatusRunning, -1, -1, "laptop"),
		}
		for i, line := range strings.Split(m.View(), "\n") {
			// The hint bar sits outside the frame and is deliberately short.
			if i == len(strings.Split(m.View(), "\n"))-1 {
				continue
			}
			if got := theme.Width(line); got != w {
				t.Errorf("%dx%d line %d is %d columns, want %d:\n  %q", w, h, i, got, w, line)
			}
		}
	}
}

func TestFilterIsAnnouncedInTheBorderOnlyWhenSet(t *testing.T) {
	m := testModel(100, 20)
	if strings.Contains(m.renderTop(98), "·") {
		t.Error("an unfiltered picker should not draw a stray status dot")
	}
	m.statusIdx = 1 // running
	top := m.renderTop(98)
	if !strings.Contains(top, statusCycle[1]) {
		t.Errorf("the active filter should be named in the border: %q", top)
	}
	if theme.Width(top) != 100 {
		t.Errorf("border with a filter is %d columns, want 100", theme.Width(top))
	}
}

// ^F cycles rather than toggles, so the hint has to say where the next press
// lands. "filter" told you a filter existed and nothing else.
func TestFilterHintNamesTheNextState(t *testing.T) {
	m := testModel(100, 20)
	want := []string{"running", "failed", "orphaned", "all"}
	for i, w := range want {
		m.statusIdx = i
		hints := m.renderHints()
		if !strings.Contains(hints, w) {
			t.Errorf("on %q the hint should offer %q: %s", statusCycle[i], w, hints)
		}
		// The active filter belongs in the border, not doubled in the hint.
		if cur := statusCycle[i]; cur != "" && strings.Contains(hints, cur) && cur != w {
			t.Errorf("hint names the current filter %q as well as the next: %s", cur, hints)
		}
	}
}

func detailOf(t *testing.T, m *Model) string {
	t.Helper()
	_, rw := m.paneWidths()
	return strings.Join(m.renderDetail(rw), "\n")
}

// The pane exists to show the command the row had to cut. It used to cap at
// four wrapped lines and drop the rest with no sign, so a long command was
// truncated twice and only the row admitted it.
func TestDetailShowsTheWholeCommandOrSaysItCannot(t *testing.T) {
	long := "ffmpeg -i input.mkv -vf \"scale=1920:-2,unsharp=5:5:0.8\" -c:v libx264 " +
		"-preset slow -crf 18 -c:a aac -b:a 192k -movflags +faststart output.mp4"

	tall := testModel(120, 30)
	tall.results = []store.Command{mkCmd(long, store.StatusCompleted, 0, 10, "laptop")}
	if got := detailOf(t, tall); !strings.Contains(squash(got), "output.mp4") {
		t.Errorf("the end of the command is missing with room to show it:\n%s", got)
	}

	short := testModel(120, 12)
	short.results = []store.Command{mkCmd(long, store.StatusCompleted, 0, 10, "laptop")}
	got := plainText(detailOf(t, short))
	if strings.Contains(squash(got), "output.mp4") {
		t.Errorf("expected the command to be cut in a short pane:\n%s", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("a cut command must say so:\n%s", got)
	}
	if strings.Contains(got, "……") {
		t.Errorf("doubled ellipsis:\n%s", got)
	}
}

// A row with no duration and no exit code should say why, since that is the
// question this tool exists to answer.
func TestDetailExplainsWhyThereIsNoResult(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{store.StatusOrphaned, "the shell exited"},
		{store.StatusRunning, "still running"},
	} {
		m := testModel(120, 30)
		m.results = []store.Command{mkCmd("sleep 300", tc.status, -1, -1, "laptop")}
		if got := plainText(detailOf(t, m)); !strings.Contains(got, tc.want) {
			t.Errorf("%s pane does not mention %q:\n%s", tc.status, tc.want, got)
		}
	}
	// A finished command needs no explanation; its row carries the exit code.
	m := testModel(120, 30)
	m.results = []store.Command{mkCmd("make", store.StatusCompleted, 0, 10, "laptop")}
	if got := plainText(detailOf(t, m)); strings.Contains(got, "still running") ||
		strings.Contains(got, "no result") {
		t.Errorf("a completed command was given an explanation it does not need:\n%s", got)
	}
}

// The session block used to be labelled only "same session", which left it
// unsaid whether those commands ran before or after. Showing the selected one
// among them settles it.
func TestSessionTimelineMarksTheSelectedCommand(t *testing.T) {
	m := testModel(120, 30)
	m.results = []store.Command{mkCmd("npm run dev", store.StatusRunning, -1, -1, "laptop")}
	m.before = []store.Command{mkCmd("git status", store.StatusCompleted, 0, 5, "laptop")}
	m.after = []store.Command{mkCmd("make deploy", store.StatusCompleted, 0, 5, "laptop")}

	got := plainText(detailOf(t, m))
	iBefore := strings.Index(got, "git status")
	iSel := strings.LastIndex(got, "npm run dev")
	iAfter := strings.Index(got, "make deploy")
	if iBefore < 0 || iSel < 0 || iAfter < 0 {
		t.Fatalf("timeline incomplete:\n%s", got)
	}
	if !(iBefore < iSel && iSel < iAfter) {
		t.Errorf("timeline is not in the order the commands ran:\n%s", got)
	}
}

func TestEmptyStatesAreDistinguished(t *testing.T) {
	m := testModel(100, 20)
	if !strings.Contains(strings.Join(m.renderList(50), ""), "nothing recorded yet") {
		t.Error("an empty database should say so")
	}
	m.query = "zzz"
	if !strings.Contains(strings.Join(m.renderList(50), ""), "no matching commands") {
		t.Error("a search with no hits should say so")
	}
}

func mkCmd(text, status string, exit int, dur int64, host string) store.Command {
	c := store.Command{
		Command: text, Status: status, Hostname: host,
		Cwd: "/home/u/app", StartTime: 1_700_000_000_000,
	}
	if exit >= 0 {
		c.ExitCode = &exit
	}
	if dur >= 0 {
		c.DurationMS = &dur
	}
	return c
}
