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

// Timestamp is when something happened, to the minute. The year is included
// only when it is not the current one: on a history that mostly covers the last
// few weeks, printing it on every line spends four columns saying the same
// thing.
func Timestamp(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms)
	if t.Year() != time.Now().Year() {
		return t.Format("Mon 2 Jan 2006, 15:04")
	}
	return t.Format("Mon 2 Jan, 15:04")
}

// Age is how long ago something started, in at most three columns, for a
// column you read down rather than a phrase you read across. RelativeTime is
// the one for prose.
//
// now is passed in rather than read from the clock so a row renders the same
// way twice.
func Age(ts, now int64) string {
	if ts <= 0 {
		return ""
	}
	d := time.Duration(now-ts) * time.Millisecond
	switch {
	// A peer whose clock is ahead of ours reports the future. "now" is a better
	// answer than a negative number.
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
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

// Safe makes recorded text printable before it reaches a terminal.
//
// A command can contain any byte its author typed, and with sync it was typed
// on another machine. Passed through as-is, an escape sequence in a command
// does what escape sequences do: `\x1b[2J` clears the screen when `shcr list`
// prints it, and a carriage return rewrites the row so what is displayed is not
// what is stored. That last one matters most in the picker, whose safety rests
// on the user seeing the command they are about to put in their prompt.
//
// Only display goes through here. The stored text and anything `shcr export`
// writes stay byte for byte as recorded.
func Safe(s string) string { return sanitize(s, false) }

// SafeMultiline is Safe but keeps newlines, for output that prints a command
// across several lines.
func SafeMultiline(s string) string { return sanitize(s, true) }

func sanitize(s string, keepNewlines bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNewlines:
			return r
		// C0, DEL, and C1 — 0x9b is an eight-bit CSI, so stripping the escape
		// character alone is not enough.
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return '\uFFFD'
		}
		return r
	}, s)
}

// FirstLine collapses a multi-line command for single-row display. The stored
// text keeps its newlines; this only affects what a list shows.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return Safe(strings.TrimRight(s[:i], " \t")) + " ↵"
	}
	return Safe(s)
}

// Truncate cuts plain text to a display width, marking the cut.
//
// Columns, not runes: a CJK glyph or an emoji occupies two cells, so counting
// runes lets a row render wider than the column it was given and push the
// picker's border off the screen. Graphemes rather than runes, so a combining
// mark or a ZWJ emoji sequence is never split down the middle.
func Truncate(s string, w int) string {
	s = Safe(s)
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
	// Safe, not SafeMultiline: this wraps to a fixed width inside a bordered
	// pane, and a literal newline in the middle would walk straight out of it.
	s = Safe(s)
	if w <= 0 {
		return nil
	}
	var out []string
	var cur strings.Builder
	used := 0

	flush := func() bool {
		out = append(out, cur.String())
		cur.Reset()
		used = 0
		return maxLines > 0 && len(out) >= maxLines
	}
	// Breaking mid-word split `npm run dev` across lines as "npm ru" / "n dev",
	// which reads as a different command. Words move whole where they fit; one
	// too long for a line of its own — a path, a URL — still has to be cut.
	for _, word := range splitKeepingSpaces(s) {
		ww := Width(word)
		if used > 0 && used+ww > w {
			if flush() {
				return out
			}
			// A space only exists to separate words; carrying it to the start of
			// the next line would indent it for no reason.
			if strings.TrimSpace(word) == "" {
				continue
			}
		}
		for used+ww > w {
			// Longer than a whole line: take what fits and carry the rest. A cut,
			// not a truncation — an ellipsis here would drop the characters it
			// stands for, and this text is going to be read in full over the next
			// few lines.
			head := cutTo(word, w-used)
			if head == "" {
				break
			}
			cur.WriteString(head)
			if flush() {
				return out
			}
			word = word[len(head):]
			ww = Width(word)
		}
		cur.WriteString(word)
		used += ww
	}
	return append(out, cur.String())
}

// cutTo returns the longest prefix of s that fits in w columns, adding nothing.
func cutTo(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		cw := g.Width()
		if used+cw > w {
			break
		}
		b.WriteString(g.Str())
		used += cw
	}
	return b.String()
}

// splitKeepingSpaces breaks text into words and the runs of spaces between
// them, so the wrapper can decide where a line ends without losing anything.
func splitKeepingSpaces(s string) []string {
	var out []string
	start, inSpace := 0, false
	for i, r := range s {
		if isSpace := r == ' '; isSpace != inSpace {
			if i > start {
				out = append(out, s[start:i])
			}
			start, inSpace = i, isSpace
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
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
		return t.body(text)
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
			b.WriteString(t.body(text[i:j]))
		}
		i = j
	}
	return b.String()
}
