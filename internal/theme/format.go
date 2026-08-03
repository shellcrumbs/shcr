package theme

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// Duration renders an elapsed time at a precision that stays scannable in a
// column: never more than four significant characters.
func Duration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

var homeDir, _ = os.UserHomeDir()

// ShortenPath replaces the home directory with ~.
func ShortenPath(p string) string {
	if homeDir == "" || p == "" {
		return p
	}
	if p == homeDir {
		return "~"
	}
	if rel, err := filepath.Rel(homeDir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

// TailPath trims a path from the left when it will not fit, keeping the end —
// the last couple of segments are what identify a directory.
func TailPath(p string, w int) string {
	if w <= 0 {
		return ""
	}
	if uniseg.StringWidth(p) <= w {
		return p
	}
	if w == 1 {
		return "…"
	}
	// Collect graphemes so the trim can count columns from the right.
	var cells []string
	var widths []int
	g := uniseg.NewGraphemes(p)
	for g.Next() {
		cells = append(cells, g.Str())
		widths = append(widths, g.Width())
	}
	used, start := 0, len(cells)
	for i := len(cells) - 1; i >= 0; i-- {
		if used+widths[i] > w-1 {
			break
		}
		used += widths[i]
		start = i
	}
	return "…" + strings.Join(cells[start:], "")
}

func RelativeTime(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hr ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return time.UnixMilli(ms).Format("2 Jan 2006")
}

// FirstLine collapses a multi-line command for single-row display. The stored
// text keeps its newlines; this only affects what a list shows.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], " \t") + " ↵"
	}
	return s
}

// Truncate cuts plain text to a display width, marking the cut.
//
// Columns, not runes: a CJK glyph or an emoji occupies two cells, so counting
// runes lets a row render wider than the column it was given and push the
// picker's border off the screen. Graphemes rather than runes, so a combining
// mark or a ZWJ emoji sequence is never split down the middle.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if uniseg.StringWidth(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		cw := g.Width()
		if used+cw > w-1 {
			break
		}
		b.WriteString(g.Str())
		used += cw
	}
	return b.String() + "…"
}

// Wrap breaks text into lines of at most w columns, again by grapheme rather
// than by rune. maxLines caps the number of lines returned; anything past that
// is dropped, with no marker to say so.
func Wrap(s string, w, maxLines int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	var cur strings.Builder
	used := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		cw := g.Width()
		if used+cw > w {
			out = append(out, cur.String())
			cur.Reset()
			used = 0
			if maxLines > 0 && len(out) >= maxLines {
				return out
			}
		}
		cur.WriteString(g.Str())
		used += cw
	}
	return append(out, cur.String())
}

// Pad extends a possibly-styled string to an exact display width.
func Pad(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// Tokens splits a search query the same way the FTS tokenizer does, so what is
// highlighted matches what was searched for.
func Tokens(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127)
	}) {
		out = append(out, strings.ToLower(f))
	}
	return out
}

// Highlight marks every occurrence of each token so the user can see why a row
// matched.
func (t *Theme) Highlight(text string, tokens []string) string {
	if len(tokens) == 0 || text == "" {
		return text
	}
	// Lowercasing does not preserve length — U+212A KELVIN SIGN is three bytes
	// and folds to a one-byte "k" — so an offset found in the folded text does
	// not address the same character in the original. orig maps every byte of
	// the folded text back to the byte where its character begins in text,
	// with a sentinel for the end, which also keeps every slice below on a
	// rune boundary.
	var lb strings.Builder
	lb.Grow(len(text))
	orig := make([]int, 0, len(text)+1)
	for i, r := range text {
		before := lb.Len()
		lb.WriteRune(unicode.ToLower(r))
		for n := lb.Len() - before; n > 0; n-- {
			orig = append(orig, i)
		}
	}
	orig = append(orig, len(text))
	lower := lb.String()

	mark := make([]bool, len(text))
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		for from := 0; from < len(lower); {
			i := strings.Index(lower[from:], tok)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(tok)
			for j := orig[start]; j < orig[end]; j++ {
				mark[j] = true
			}
			from = end
		}
	}

	var b strings.Builder
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && mark[j] == mark[i] {
			j++
		}
		if mark[i] {
			b.WriteString(t.Match.Render(text[i:j]))
		} else {
			b.WriteString(text[i:j])
		}
		i = j
	}
	return b.String()
}
