package theme

import (
	"strconv"
	"strings"
	"time"

	"github.com/shellcrumbs/shcr/internal/store"
)

// RowOpts describes how one history row should be rendered. The picker and
// `shcr list` differ only in these values, which is what keeps the two surfaces
// looking like one product.
type RowOpts struct {
	// Width is the exact number of columns the row must occupy.
	Width int
	// Selected draws the cursor marker and emphasises the command.
	Selected bool
	// ShowTime prefixes the wall-clock start time. `shcr list` reads as a log,
	// so it wants this; the picker does not, because its detail pane says when.
	ShowTime bool
	// Tokens are highlighted inside the command text.
	Tokens []string
	// LocalHost suppresses the host chip for commands that ran here. Empty means
	// never show the host.
	LocalHost string
	// BaseCwd suppresses the directory for commands that ran there. Empty means
	// never show the directory, which is what the picker wants.
	BaseCwd string
	// Now, in unix millis, lets a running command show how long it has been
	// going. Zero leaves the slot blank, which is what a row rendered for a
	// fixed point in time — a test, a snapshot — wants.
	Now int64
	// ReserveHost keeps the host slot at a fixed width even on rows that do not
	// use it. Without it the command text ends at a different column on every
	// row, because the slot is as wide as that row's hostname.
	ReserveHost bool
	// ShowAge puts how long ago the command ran at the end of the row. The
	// picker wants it — with no clock column, two similar commands are told
	// apart by when they ran. `shcr list` already prints the time itself.
	ShowAge bool

	// selected is the same flag as Selected, after Row has switched to the
	// selection variant. It draws the marker and the bold text without sending
	// the row round the switch a second time.
	selected bool
}

const (
	// Below this many columns for the command itself, chips start being dropped —
	// a row that is all metadata and no command helps nobody.
	minCommandWidth = 16
	// Fixed slots so durations and exit codes read as columns. Wide enough for
	// the chip padding around "1h 30m" and "127".
	ageSlot      = 4
	hostSlot     = 12
	durationSlot = 9
	exitSlot     = 5
)

// Row renders one command as a single line of exactly o.Width columns.
//
//	[time]  {dot}  {command ..................}  {cwd}  {chips}
//
// Chips are right-aligned as a group so the eye can scan durations and exit
// codes down a column, and they drop right-to-left as space runs out.
func (t *Theme) Row(c store.Command, o RowOpts) string {
	if o.Width <= 0 {
		return ""
	}
	// Everything below is drawn through the selection variant, so every piece of
	// the row — text, chips, padding, the gaps between them — carries the same
	// background and the band comes out in one piece.
	if o.Selected && t.sel != nil {
		return t.sel.Row(c, RowOpts{Width: o.Width, Selected: false, ShowTime: o.ShowTime,
			Tokens: o.Tokens, LocalHost: o.LocalHost, BaseCwd: o.BaseCwd, Now: o.Now,
			ReserveHost: o.ReserveHost, ShowAge: o.ShowAge, selected: true})
	}

	prefix := t.prefix(c, o, o.ShowTime)
	// On a very narrow terminal the prefix alone can overrun the line. Give up
	// the timestamp first, then the prefix entirely, rather than overflow — a
	// row wider than its column would corrupt the picker's frame.
	if Width(prefix)+minCommandWidth/2 > o.Width && o.ShowTime {
		prefix = t.prefix(c, o, false)
	}
	prefixW := Width(prefix)
	if prefixW >= o.Width {
		return t.padTo(t.body(Truncate(FirstLine(c.Command), o.Width)), o.Width)
	}

	// Build the trailing group, then give the command whatever is left. Each
	// pass drops the least important remaining element.
	for attempt := range 4 {
		trailing := t.trailing(c, o, attempt)
		trailingW := Width(trailing)
		gap := 0
		if trailingW > 0 {
			gap = 2
		}
		cmdW := o.Width - prefixW - trailingW - gap
		if cmdW < minCommandWidth && attempt < 3 {
			continue
		}
		if cmdW < 1 {
			cmdW = 1
		}

		text := Truncate(FirstLine(c.Command), cmdW)
		styled := t.Highlight(text, o.Tokens)

		line := prefix + styled
		if trailingW > 0 {
			// Padding out to exactly Width-trailingW is what right-aligns the
			// group, so durations and exit codes line up down the column.
			line = t.padTo(line, o.Width-trailingW) + trailing
		}
		return t.padTo(line, o.Width)
	}
	// Unreachable: the last attempt cannot `continue`. Go cannot see that, and
	// a function ending in a `for` still needs a return.
	return t.padTo(prefix, o.Width)
}

// trailing builds the right-hand group. Each successive level drops one more
// element, in reverse order of usefulness: the exit code identifies a failure,
// the duration is what you scan for, the host and directory are context you
// often already know.
func (t *Theme) trailing(c store.Command, o RowOpts, level int) string {
	var parts []string

	// Directory, only when it differs from where the caller is standing.
	if level < 1 && o.BaseCwd != "" && c.Cwd != "" && c.Cwd != o.BaseCwd {
		if p := ShortenPath(c.Cwd); p != "" {
			parts = append(parts, t.Muted.Render(TailPath(p, 24)))
		}
	}
	// Host, only when the command ran somewhere else. On a single machine this
	// column stays empty, which is the point.
	if level < 2 && o.LocalHost != "" {
		host := ""
		if c.Hostname != "" && c.Hostname != o.LocalHost {
			host = t.chipInfo.Render(Pad(Truncate(c.Hostname, hostSlot), hostSlot))
		}
		if host != "" || o.ReserveHost {
			parts = append(parts, t.padLeft(host, hostSlot+2))
		}
	}
	// Duration and exit code get fixed-width slots, blank-filled when absent.
	// Right-aligning the group as a whole would let a row with an exit code
	// shove its neighbour's duration sideways, and a duration column you cannot
	// read down is not worth having.
	if level < 3 {
		// A finished command shows how long it took; one still running shows how
		// long it has been going. Leaving that blank made a running row — the
		// thing this tool exists to show — carry less than a finished one.
		// Orphaned stays blank on purpose: nobody knows when it ended.
		dur := ""
		switch {
		case c.DurationMS != nil:
			dur = t.chipInfo.Render(Duration(*c.DurationMS))
		case c.Status == store.StatusRunning && o.Now > c.StartTime && c.StartTime > 0:
			dur = t.chipInfo.Render(Duration(o.Now - c.StartTime))
		}
		parts = append(parts, t.padLeft(dur, durationSlot))

		// The exit code only earns ink when it is not zero: a green tick already
		// says "exit 0", and a column of zeroes buries the 127 you are looking
		// for. The slot is still reserved so the column stays straight.
		exit := ""
		if c.ExitCode != nil && *c.ExitCode != 0 {
			exit = t.chipExit.Render(strconv.Itoa(*c.ExitCode))
		}
		parts = append(parts, t.padLeft(exit, exitSlot))

		if o.ShowAge && o.Now > 0 {
			// One column of the slot is margin: the trailing group sits flush
			// against the frame, and a column of text touching the border reads
			// as though it has been cut off.
			parts = append(parts, t.padLeft(t.Muted.Render(Age(c.StartTime, o.Now)), ageSlot-1)+t.body(" "))
		}
	} else if c.ExitCode != nil && *c.ExitCode != 0 {
		// Last resort: no room for columns, but a failure still says which one.
		parts = append(parts, t.chipExit.Render(strconv.Itoa(*c.ExitCode)))
	}
	return strings.Join(parts, t.body(" "))
}

// padLeft right-aligns a possibly-styled string inside a fixed field.
func (t *Theme) padLeft(s string, w int) string {
	if d := w - Width(s); d > 0 {
		return t.body(strings.Repeat(" ", d)) + s
	}
	return s
}

// prefix is the fixed left-hand part: selection marker, optional clock, dot.
func (t *Theme) prefix(c store.Command, o RowOpts, withTime bool) string {
	// The leading space keeps the selection bar off the picker's frame border,
	// which otherwise reads as one thick line rather than a cursor.
	s := t.body("   ")
	if o.selected {
		s = t.body(" ") + t.Accent.Render("▌") + t.body(" ")
	}
	if withTime {
		s += t.Muted.Render(time.UnixMilli(c.StartTime).Format("15:04:05")) + t.body("  ")
	}
	return s + t.Mark(c) + t.body("  ")
}
