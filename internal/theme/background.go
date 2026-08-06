package theme

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// backgroundBudget is what asking the terminal its colour is allowed to cost.
// Terminals that answer do so in about a millisecond; this only ever elapses on
// one that will not answer at all, and it is charged once per process, before
// the first frame.
const backgroundBudget = 150 * time.Millisecond

// queryDarkBackground asks the terminal for its background colour with OSC 11
// and reports whether it is dark. ok is false when the terminal did not answer,
// which is the normal case for a pipe, a CI job, or a multiplexer that filters
// the query.
//
// termenv can do this, but its timeout is a five-second constant, and a picker
// that is allowed 50ms to draw cannot spend five seconds deciding what colour
// to draw in. Same exchange, with a budget.
//
// Guessing was survivable while the palette only tinted foregrounds over the
// terminal's own background. A selected row paints a background of its own, and
// there a wrong guess is the difference between a highlight and a line you
// cannot read.
func queryDarkBackground(f *os.File, within time.Duration) (dark, ok bool) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return false, false
	}
	// The same exclusions termenv makes. A multiplexer may swallow the query
	// rather than forward it, and a CI job has no terminal to ask — in both
	// cases the budget would be spent every time for an answer that never comes.
	if os.Getenv("CI") != "" {
		return false, false
	}
	switch t := os.Getenv("TERM"); {
	case strings.HasPrefix(t, "screen"), strings.HasPrefix(t, "tmux"), strings.HasPrefix(t, "dumb"), t == "":
		return false, false
	}
	// Raw mode, so the answer arrives as bytes rather than being interpreted,
	// and is not echoed onto the screen the picker is about to draw on.
	old, err := term.MakeRaw(fd)
	if err != nil {
		return false, false
	}
	defer func() { _ = term.Restore(fd, old) }()

	if _, err := f.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}

	deadline := time.Now().Add(within)
	var buf []byte
	for len(buf) < 128 {
		left := time.Until(deadline)
		if left <= 0 || !readable(fd, left) {
			break
		}
		chunk := make([]byte, 64)
		n, err := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
		// The reply ends at BEL or ST; stopping there means we do not sit out
		// the rest of the budget once the answer is already in hand.
		if strings.ContainsAny(string(buf), "\a") || strings.Contains(string(buf), "\x1b\\") {
			break
		}
	}
	return parseOSC11(string(buf))
}

// readable waits until fd has something to read, or the timeout expires.
func readable(fd int, timeout time.Duration) bool {
	var set unix.FdSet
	set.Set(fd)
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	for {
		n, err := unix.Select(fd+1, &set, nil, nil, &tv)
		if err == unix.EINTR {
			continue
		}
		return err == nil && n > 0
	}
}

// parseOSC11 reads the terminal's reply to an OSC 11 query, which looks like
//
//	ESC ] 11 ; rgb:1e1e/1e1e/1e1e BEL
//
// and reports whether that colour is dark. Components are one to four hex
// digits and have to be scaled by their own width, not assumed to be 16-bit.
func parseOSC11(s string) (dark, ok bool) {
	i := strings.Index(s, "rgb:")
	if i < 0 {
		return false, false
	}
	rest := s[i+len("rgb:"):]
	if cut := strings.IndexAny(rest, "\a\x1b"); cut >= 0 {
		rest = rest[:cut]
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return false, false
	}
	var c [3]float64
	for n, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 4 {
			return false, false
		}
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return false, false
		}
		c[n] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	// The same test termenv makes: HSL lightness, which is the midpoint of the
	// brightest and dimmest channel.
	lo, hi := c[0], c[0]
	for _, v := range c[1:] {
		lo, hi = min(lo, v), max(hi, v)
	}
	return (lo+hi)/2 < 0.5, true
}
