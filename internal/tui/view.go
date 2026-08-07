package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

// Lines the frame itself costs: top border, query, separator, bottom border,
// hint bar.
const chromeHeight = 5

func (m *Model) bodyHeight() int {
	return max(m.height-chromeHeight, 3)
}

// One row per command, so a normal terminal shows twenty-odd commands instead
// of half a dozen. Everything a row used to spend a second line on is either a
// chip on the same line or in the detail pane.
func (m *Model) visibleItems() int { return m.bodyHeight() }

func (m *Model) split() bool { return m.width >= minSplitWidth }

func (m *Model) paneWidths() (left, right int) {
	inner := m.width - 2
	if !m.split() {
		return inner, 0
	}
	// Capped, not proportional. The pane's widest real line is a hostname
	// beside its label; letting it take 45% of a wide terminal spent the space
	// on padding and took it from the command text, so a 120-column terminal
	// showed less of a command than an 80-column one.
	right = min(inner*45/100, maxDetailWidth)
	return inner - right - 1, right
}

// minFrameWidth and minFrameHeight are the smallest frame worth drawing: below
// them the borders and the prompt leave no room for a command.
const (
	minFrameWidth  = 24
	minFrameHeight = 8
)

// renderTooSmall fills the screen rather than printing a bare line into it.
// Bubble Tea diffs frames, so a message shorter than the last frame leaves the
// remains of that frame around it — which on a window being dragged looks like
// the picker has broken rather than that it wants more room.
func (m *Model) renderTooSmall() string {
	w, h := max(m.width, 1), max(m.height, 1)
	msg := theme.Truncate(fmt.Sprintf("needs %d×%d", minFrameWidth, minFrameHeight), w)

	var b strings.Builder
	for row := range h {
		if row > 0 {
			b.WriteByte('\n')
		}
		if row != h/2 {
			b.WriteString(strings.Repeat(" ", w))
			continue
		}
		pad := (w - theme.Width(msg)) / 2
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(m.theme.Muted.Render(msg))
		b.WriteString(strings.Repeat(" ", w-pad-theme.Width(msg)))
	}
	return b.String()
}

func (m *Model) View() string {
	if m.width < minFrameWidth || m.height < minFrameHeight {
		return m.renderTooSmall()
	}
	inner := m.width - 2
	leftW, rightW := m.paneWidths()

	var b strings.Builder
	b.WriteString(m.renderTop(inner))
	b.WriteByte('\n')
	b.WriteString(m.renderQuery(inner))
	b.WriteByte('\n')
	b.WriteString(m.renderSeparator(leftW, rightW))
	b.WriteByte('\n')

	list := m.renderList(leftW)
	var detail []string
	if m.split() {
		detail = m.renderDetail(rightW)
	}
	for i := range m.bodyHeight() {
		b.WriteString(m.theme.Frame.Render("│"))
		b.WriteString(theme.Pad(at(list, i), leftW))
		if m.split() {
			b.WriteString(m.theme.Frame.Render("│"))
			b.WriteString(theme.Pad(at(detail, i), rightW))
		}
		b.WriteString(m.theme.Frame.Render("│"))
		b.WriteByte('\n')
	}

	b.WriteString(m.renderBottom(leftW, rightW))
	b.WriteByte('\n')
	b.WriteString(m.renderHints())
	return b.String()
}

func (m *Model) renderTop(inner int) string {
	title := " shellcrumbs "
	// How many rows matched, but only while filtering: unfiltered the list is
	// capped at refineCandidates, so a count would describe the page and not
	// the history.
	count := ""
	if m.query != "" || statusCycle[m.statusIdx] != "" {
		switch n := len(m.results); {
		case n >= refineCandidates:
			count = m.theme.Muted.Render(fmt.Sprintf("%d+ matches ", refineCandidates))
		case n == 1:
			count = m.theme.Muted.Render("1 match ")
		default:
			count = m.theme.Muted.Render(fmt.Sprintf("%d matches ", n))
		}
	}
	// The active filter is announced in the border, with the same dot the rows
	// use. With no filter there is nothing to say, so nothing is drawn.
	right := ""
	if f := statusCycle[m.statusIdx]; f != "" {
		right = " " + m.theme.Dot(f) + m.theme.Muted.Render(" "+f) + " "
	}
	fill := max(inner-1-len(title)-theme.Width(count)-theme.Width(right)-1, 0)
	return m.theme.Frame.Render("╭─") +
		m.theme.Title.Render(title) +
		count +
		m.theme.Frame.Render(strings.Repeat("─", fill)) +
		right +
		m.theme.Frame.Render("─╮")
}

func (m *Model) renderQuery(inner int) string {
	// The block sits on the character it is in front of, rather than always at
	// the end: with the caret movable, drawing it at the end would say the
	// text is being appended to when it is being edited in the middle.
	r := []rune(theme.Truncate(m.query, inner-4))
	caret := max(0, min(m.caret, len(r)))
	var typed string
	switch {
	case caret >= len(r):
		typed = string(r) + m.theme.Match.Render("▊")
	default:
		typed = string(r[:caret]) +
			m.theme.Cursor.Render(string(r[caret])) +
			string(r[caret+1:])
	}
	line := " " + m.theme.Title.Render("❯ ") + typed
	return m.theme.Frame.Render("│") + theme.Pad(line, inner) + m.theme.Frame.Render("│")
}

func (m *Model) renderSeparator(leftW, rightW int) string {
	if !m.split() {
		return m.theme.Frame.Render("├" + strings.Repeat("─", leftW) + "┤")
	}
	return m.theme.Frame.Render("├" + strings.Repeat("─", leftW) + "┬" + strings.Repeat("─", rightW) + "┤")
}

func (m *Model) renderBottom(leftW, rightW int) string {
	if !m.split() {
		return m.theme.Frame.Render("╰" + strings.Repeat("─", leftW) + "╯")
	}
	return m.theme.Frame.Render("╰" + strings.Repeat("─", leftW) + "┴" + strings.Repeat("─", rightW) + "╯")
}

// nextFilterLabel names what ^F will do, not what it did. The filter is a
// cycle, and a key labelled "filter" says only that one exists — from an
// unfiltered list you cannot tell what pressing it gives you, and from a
// filtered one you cannot tell whether the next press moves on or clears.
//
// Which filter is active is already in the border, with the same dot the rows
// use, so naming the next one here adds to that rather than repeating it.
func nextFilterLabel(idx int) string {
	next := statusCycle[(idx+1)%len(statusCycle)]
	if next == "" {
		return "all"
	}
	return next
}

func (m *Model) renderHints() string {
	pairs := [][2]string{
		{"↑↓", "navigate"},
		{"⏎", "insert"},
		{"^Y", "copy"},
		{"^F", nextFilterLabel(m.statusIdx)},
		{"esc", "cancel"},
	}
	if m.copied {
		pairs[2] = [2]string{"^Y", "copied!"}
	}
	var parts []string
	for _, p := range pairs {
		parts = append(parts, m.theme.Label.Bold(true).Render(p[0])+" "+m.theme.Muted.Render(p[1]))
	}
	return "  " + strings.Join(parts, m.theme.Muted.Render("  "))
}

func (m *Model) renderList(w int) []string {
	out := make([]string, 0, m.bodyHeight())
	if m.err != nil {
		return append(out, " "+m.theme.Accent.Foreground(theme.StatusColor("failed")).
			Render("query error: "+m.err.Error()))
	}
	if len(m.results) == 0 {
		msg := " no matching commands"
		if m.query == "" && statusCycle[m.statusIdx] == "" {
			msg = " nothing recorded yet"
		}
		return append(out, m.theme.Muted.Render(msg))
	}

	tokens := theme.Tokens(m.query)
	now := time.Now().UnixMilli()
	// One row from another machine is enough to reserve the column on all of
	// them, so the command text ends at the same place down the list.
	reserveHost := false
	for i := m.offset; i < len(m.results) && i < m.offset+m.bodyHeight(); i++ {
		if h := m.results[i].Hostname; h != "" && h != m.localHost {
			reserveHost = true
			break
		}
	}
	for i := m.offset; i < len(m.results) && len(out) < m.bodyHeight(); i++ {
		out = append(out, m.theme.Row(m.results[i], theme.RowOpts{
			Now:         now,
			ReserveHost: reserveHost,
			ShowAge:     true,
			Width:       w,
			Selected:    i == m.cursor,
			Tokens:      tokens,
			LocalHost:   m.localHost,
			// No directory here: the detail pane carries it, and a path on every
			// row would crowd out the command.
		}))
	}
	return out
}

func (m *Model) renderDetail(w int) []string {
	c := m.selected()
	if c == nil {
		return nil
	}
	pw := w - 2
	budget := m.bodyHeight()

	// Everything except the command is measured first, because the command is
	// what the pane is for: it gets whatever is left rather than a fixed four
	// lines with the rest dropped on the floor.
	meta := m.detailMeta(*c, pw)
	session := m.detailSession(pw)
	room := budget - len(meta) - len(session)
	if len(session) > 0 {
		room-- // the blank line above the session block
	}

	out := make([]string, 0, budget)
	lines := theme.Wrap(c.Command, pw, 0)
	if room < 1 {
		room = 1
	}
	if len(lines) > room {
		// Say so, rather than stopping mid-word and looking complete. Truncate
		// adds its own ellipsis when it shortens, so only add one when it did
		// not — two in a row reads as a typo.
		lines = lines[:room]
		last := theme.Truncate(lines[room-1], pw-1)
		if !strings.HasSuffix(last, "…") {
			last += "…"
		}
		lines[room-1] = last
	}
	for _, line := range lines {
		out = append(out, " "+m.theme.Highlight(line, theme.Tokens(m.query)))
	}
	out = append(out, meta...)
	if len(session) > 0 && len(out)+len(session) < budget {
		out = append(out, "")
		out = append(out, session...)
	}
	return out
}

// detailMeta is the context a chip has no room for. Status, duration and exit
// stay in the selected row, a few columns to the left.
func (m *Model) detailMeta(c store.Command, pw int) []string {
	out := []string{""}

	// Why a row has no duration and no exit code. The glyph in the list says
	// which state it is; this says what that state means for the result, which
	// is the question the picker exists to answer.
	switch {
	case c.Status == store.StatusOrphaned:
		out = append(out, " "+m.theme.Muted.Render(theme.Truncate("no result: the shell exited first", pw)))
	case c.Status == store.StatusRunning:
		out = append(out, " "+m.theme.Muted.Render("still running"))
	case c.Imported:
		out = append(out, " "+m.theme.Muted.Render(theme.Truncate("imported: no exit code recorded", pw)))
	}

	// What this row stands for. The list shows one entry per command, so a row
	// backed by fifty executions is indistinguishable from one backed by a
	// single run — and the count is part of why it is placed where it is.
	if st, ok := m.statIndex[c.Command]; ok {
		if summary := st.Summary(); summary != "" {
			out = append(out, " "+m.theme.Muted.Render(theme.Truncate(summary, pw)))
		}
	}

	// Each row only when it has something to say. An imported command carries
	// no directory, and a label with nothing after it spends a line saying so.
	var rows [][2]string
	if c.Hostname != "" {
		rows = append(rows, [2]string{"host", c.Hostname})
	}
	if d := theme.ShortenPath(c.Cwd); d != "" {
		rows = append(rows, [2]string{"dir", d})
	}
	// Only when there is one. A row reading "branch —" spends a line saying
	// nothing.
	if c.GitBranch != nil && *c.GitBranch != "" {
		rows = append(rows, [2]string{"branch", *c.GitBranch})
	}
	// The absolute time, because the row already carries how long ago it was.
	// Two ways of saying "5 minutes" would leave neither saying when.
	rows = append(rows, [2]string{"started", theme.Timestamp(c.StartTime)})
	for _, r := range rows {
		out = append(out, " "+m.theme.Label.Render(theme.Pad(r[0], 9))+theme.Truncate(r[1], pw-10))
	}
	return out
}

// detailSession renders the shell session around the selected command as one
// timeline with the command marked in place. A list labelled only "same
// session" left it unsaid whether those commands came before or after; showing
// the selected one among them answers that without a word.
func (m *Model) detailSession(pw int) []string {
	c := m.selected()
	if c == nil || (len(m.before) == 0 && len(m.after) == 0) {
		return nil
	}
	out := []string{" " + m.theme.Label.Render("session")}
	line := func(text string, current bool) string {
		t := theme.Truncate(theme.FirstLine(text), pw-3)
		if current {
			return " " + m.theme.Accent.Render("▌") + " " + m.theme.Accent.Render(t)
		}
		return "   " + m.theme.Muted.Render(t)
	}
	// before is newest-first from the query; read it backwards so the whole
	// block runs in the order the commands were typed.
	for i := len(m.before) - 1; i >= 0; i-- {
		out = append(out, line(m.before[i].Command, false))
	}
	out = append(out, line(c.Command, true))
	for _, n := range m.after {
		out = append(out, line(n.Command, false))
	}
	return out
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
