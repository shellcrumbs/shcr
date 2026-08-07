package tui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

// The picker is where you fix a typo in a command you half-remember, so the
// query line has to behave like a line of text and not like an append-only log.
func TestTheQueryCanBeEditedInPlace(t *testing.T) {
	m := New(nil, nil, "")
	press := func(keys ...string) {
		for _, k := range keys {
			m.Update(keyMsg(k))
		}
	}
	type step struct {
		keys  []string
		query string
		caret int
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"typing puts the caret after the text", []step{
			{[]string{"n", "p", "m"}, "npm", 3},
		}},
		{"left and right move without changing anything", []step{
			{[]string{"n", "p", "m"}, "npm", 3},
			{[]string{"left", "left"}, "npm", 1},
			{[]string{"right"}, "npm", 2},
		}},
		{"typing inserts at the caret", []step{
			{[]string{"g", "i", "t"}, "git", 3},
			{[]string{"left", "left", "left"}, "git", 0},
			{[]string{"s", "u", "d", "o", " "}, "sudo git", 5},
		}},
		{"backspace takes the character before the caret", []step{
			{[]string{"a", "b", "c"}, "abc", 3},
			{[]string{"left"}, "abc", 2},
			{[]string{"backspace"}, "ac", 1},
		}},
		{"delete takes the one under it", []step{
			{[]string{"a", "b", "c"}, "abc", 3},
			{[]string{"home"}, "abc", 0},
			{[]string{"delete"}, "bc", 0},
		}},
		{"home and end reach both ends", []step{
			{[]string{"a", "b", "c"}, "abc", 3},
			{[]string{"home"}, "abc", 0},
			{[]string{"end"}, "abc", 3},
			{[]string{"ctrl+a"}, "abc", 0},
			{[]string{"ctrl+e"}, "abc", 3},
		}},
		{"ctrl+w deletes a word, and the spaces before it", []step{
			{[]string{"g", "i", "t", " ", "p", "u", "s", "h"}, "git push", 8},
			{[]string{"ctrl+w"}, "git ", 4},
			{[]string{"ctrl+w"}, "", 0},
		}},
		{"ctrl+w from the middle takes only what is behind the caret", []step{
			{[]string{"g", "i", "t", " ", "p", "u", "s", "h"}, "git push", 8},
			{[]string{"left", "left", "left", "left"}, "git push", 4},
			{[]string{"ctrl+w"}, "push", 0},
		}},
		{"ctrl+u clears back to the start", []step{
			{[]string{"g", "i", "t", " ", "p", "u", "s", "h"}, "git push", 8},
			{[]string{"left", "left", "left", "left"}, "git push", 4},
			{[]string{"ctrl+u"}, "push", 0},
		}},
		{"the caret cannot leave the text", []step{
			{[]string{"a"}, "a", 1},
			{[]string{"right", "right", "right"}, "a", 1},
			{[]string{"left", "left", "left"}, "a", 0},
			{[]string{"backspace", "backspace"}, "a", 0},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m = New(nil, nil, "")
			for i, st := range tc.steps {
				press(st.keys...)
				if m.query != st.query || m.caret != st.caret {
					t.Fatalf("step %d %v: query=%q caret=%d, want %q %d",
						i, st.keys, m.query, m.caret, st.query, st.caret)
				}
			}
		})
	}
}

// Ctrl+R with something already typed hands the picker that text; the caret
// belongs after it, ready to carry on.
func TestAPreTypedQueryStartsWithTheCaretAtTheEnd(t *testing.T) {
	m := New(nil, nil, "npm run")
	if m.caret != len([]rune("npm run")) {
		t.Errorf("caret at %d, want %d", m.caret, len([]rune("npm run")))
	}
}

// keyMsg builds the message Bubble Tea would deliver for a key, so the tests go
// through the same Update the picker does rather than round a helper.
func keyMsg(k string) tea.KeyMsg {
	named := map[string]tea.KeyType{
		"left": tea.KeyLeft, "right": tea.KeyRight,
		"home": tea.KeyHome, "end": tea.KeyEnd,
		"ctrl+a": tea.KeyCtrlA, "ctrl+e": tea.KeyCtrlE,
		"ctrl+w": tea.KeyCtrlW, "ctrl+u": tea.KeyCtrlU,
		"ctrl+k":    tea.KeyCtrlK,
		"backspace": tea.KeyBackspace, "delete": tea.KeyDelete,
		" ": tea.KeySpace,
	}
	if t, ok := named[k]; ok {
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// The wheel moves the selection, because a list you can see is a list you
// expect to be able to scroll.
func TestTheWheelMovesTheSelection(t *testing.T) {
	m := New(nil, nil, "")
	m.results = make([]store.Command, 50)
	for i := range m.results {
		m.results[i] = store.Command{ID: string(rune('a' + i%26)), Command: "c"}
	}
	m.height, m.width = 24, 100

	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if m.cursor != wheelLines {
		t.Errorf("wheel down moved to %d, want %d", m.cursor, wheelLines)
	}
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if m.cursor != 0 {
		t.Errorf("wheel up moved to %d, want 0", m.cursor)
	}
	// And it stops at the ends rather than running off them.
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if m.cursor != 0 {
		t.Errorf("wheel up past the top moved to %d", m.cursor)
	}
}

// A window too small to draw in gets a frame of its own, not a bare line left
// sitting in the remains of the previous one.
func TestTheTooSmallMessageFillsTheScreen(t *testing.T) {
	m := New(nil, theme.New(io.Discard), "")
	m.width, m.height = 20, 6
	out := m.View()
	lines := strings.Split(out, "\n")
	if len(lines) != 6 {
		t.Fatalf("drew %d lines into a 6-row terminal", len(lines))
	}
	for i, l := range lines {
		if w := theme.Width(l); w != 20 {
			t.Errorf("line %d is %d columns, want 20: %q", i, w, l)
		}
	}
	if !strings.Contains(out, "24") {
		t.Errorf("the message does not say what it needs: %q", out)
	}
}
