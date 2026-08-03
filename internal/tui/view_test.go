package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

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
