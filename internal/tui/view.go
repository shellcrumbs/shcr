package tui

import (
	"fmt"
	"strings"
	"time"

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

func (m *Model) View() string {
	if m.width < 24 || m.height < 8 {
		return "terminal too small"
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
	// capped at queryLimit, so a count would describe the page and not the
	// history.
	count := ""
	if m.query != "" || statusCycle[m.statusIdx] != "" {
		switch n := len(m.results); {
		case n >= queryLimit:
			count = m.theme.Muted.Render(fmt.Sprintf("%d+ matches ", queryLimit))
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
	line := " " + m.theme.Title.Render("❯ ") +
		theme.Truncate(m.query, inner-4) + m.theme.Match.Render("▊")
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

func (m *Model) renderHints() string {
	pairs := [][2]string{
		{"↑↓", "navigate"},
		{"⏎", "insert"},
		{"^Y", "copy"},
		{"^F", "filter"},
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
	out := make([]string, 0, m.bodyHeight())
	pw := w - 2

	for _, line := range theme.Wrap(c.Command, pw, 4) {
		out = append(out, " "+line)
	}
	out = append(out, "")

	// Status, duration and exit code all live in the selected row's chips a few
	// columns to the left, so repeating them here would be noise. What is left
	// is the context a chip has no room for.
	rows := [][2]string{
		{"host", c.Hostname},
		{"dir", theme.ShortenPath(c.Cwd)},
	}
	// Only when there is one. A row reading "branch —" spends a line saying
	// nothing.
	if c.GitBranch != nil && *c.GitBranch != "" {
		rows = append(rows, [2]string{"branch", *c.GitBranch})
	}
	rows = append(rows, [2]string{"started", theme.RelativeTime(c.StartTime)})
	for _, r := range rows {
		out = append(out, " "+m.theme.Label.Render(theme.Pad(r[0], 9))+theme.Truncate(r[1], pw-10))
	}

	if len(m.neighbors) > 0 && len(out)+3 < m.bodyHeight() {
		out = append(out, "")
		out = append(out, " "+m.theme.Frame.Render("┌ ")+m.theme.Muted.Render("same session")+" "+
			m.theme.Frame.Render(strings.Repeat("─", max(pw-16, 0))+"┐"))
		// Newest-first from the query; show them in the order they ran.
		for i := len(m.neighbors) - 1; i >= 0; i-- {
			if len(out) >= m.bodyHeight()-1 {
				break
			}
			// pw-3 keeps these rows flush with the header and footer, which are
			// one column wider than the naive content width.
			t := theme.Truncate(theme.FirstLine(m.neighbors[i].Command), pw-3)
			out = append(out, " "+m.theme.Frame.Render("│ ")+
				m.theme.Muted.Render(theme.Pad(t, pw-3))+m.theme.Frame.Render("│"))
		}
		if len(out) < m.bodyHeight() {
			out = append(out, " "+m.theme.Frame.Render("└"+strings.Repeat("─", pw-2)+"┘"))
		}
	}
	return out
}

// ---------------------------------------------------------------- helpers

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
