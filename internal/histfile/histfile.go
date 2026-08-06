// Package histfile reads the history files the shells keep for themselves.
//
// These are recovery formats, not interchange formats: each shell writes what
// suits its own reader and nothing more. Between them they lose exit codes
// entirely, lose timestamps unless the user opted in, and — in zsh's case —
// re-encode bytes in a way that produces invalid UTF-8 if taken literally.
// Everything here exists to get real text out of them without inventing
// anything that was not there.
package histfile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kind is which shell wrote the file.
type Kind string

const (
	Bash Kind = "bash"
	Zsh  Kind = "zsh"
	Fish Kind = "fish"
)

// Entry is one command recovered from a history file.
type Entry struct {
	Command string
	// StartTime is unix millis. Zero means the file carried no time.
	StartTime int64
	// Approximate is set when StartTime was derived from the entry's position
	// rather than read from the file.
	Approximate bool
	// DurationMS comes only from zsh's elapsed field, and is usually zero even
	// there.
	DurationMS int64
}

// Source is a parsed history file.
type Source struct {
	Path    string
	Kind    Kind
	Entries []Entry
}

// Discover finds the history files a user is likely to have.
func Discover() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	candidates := []string{
		filepath.Join(home, ".bash_history"),
		filepath.Join(home, ".zsh_history"),
		filepath.Join(home, ".histfile"), // a common zsh HISTFILE
		filepath.Join(home, ".local", "share", "fish", "fish_history"),
	}
	if h := os.Getenv("HISTFILE"); h != "" {
		candidates = append([]string{h}, candidates...)
	}

	seen := map[string]bool{}
	var found []string
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil || seen[abs] {
			continue
		}
		if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			seen[abs] = true
			found = append(found, abs)
		}
	}
	return found
}

// Parse reads a history file, sniffing the format from its contents.
//
// Sniffed rather than taken from the filename: HISTFILE is routinely pointed
// somewhere else, and a zsh history called .bash_history parsed as bash would
// import every command with its timestamp prefix glued to the front.
func Parse(path string) (*Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	kind := sniff(raw)
	src := &Source{Path: path, Kind: kind}
	switch kind {
	case Zsh:
		src.Entries = parseZsh(raw, info.ModTime())
	case Fish:
		src.Entries = parseFish(raw)
	default:
		src.Entries = parseBash(raw, info.ModTime())
	}
	return src, nil
}

func sniff(raw []byte) Kind {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 0; sc.Scan() && n < 40; n++ {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if zshEntry(line) != nil {
			return Zsh
		}
		if bytes.HasPrefix(line, []byte("- cmd:")) {
			return Fish
		}
	}
	return Bash
}

// ---------------------------------------------------------------- zsh

// zshEntry matches `: <started>:<elapsed>;<command>` and returns the three
// parts, or nil when the line is not an entry header.
func zshEntry(line []byte) [][]byte {
	if !bytes.HasPrefix(line, []byte(": ")) {
		return nil
	}
	rest := line[2:]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return nil
	}
	semi := bytes.IndexByte(rest[colon:], ';')
	if semi < 0 {
		return nil
	}
	semi += colon
	started, elapsed := rest[:colon], rest[colon+1:semi]
	if len(started) == 0 || !allDigits(started) || !allDigits(elapsed) {
		return nil
	}
	return [][]byte{started, elapsed, rest[semi+1:]}
}

func allDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseZsh(raw []byte, modTime time.Time) []Entry {
	var out []Entry
	lines := bytes.Split(raw, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}

		var cmd []byte
		var started, elapsed int64
		if parts := zshEntry(lines[i]); parts != nil {
			cmd = append([]byte(nil), parts[2]...)
			started, _ = strconv.ParseInt(string(parts[0]), 10, 64)
			elapsed, _ = strconv.ParseInt(string(parts[1]), 10, 64)
		} else {
			// A line with no `: <start>:<elapsed>;` prefix is a command written
			// before EXTENDED_HISTORY was switched on, which plenty of people do
			// partway through a history's life. Skipping those lines is not a
			// no-op: the file is recognised as zsh from the entries that do have
			// the prefix, so everything older than the switch disappears with no
			// sign that it was there.
			cmd = append([]byte(nil), lines[i]...)
		}

		// A trailing backslash continues the command onto the next line. This is
		// how zsh stores anything multi-line, and 100-odd of them in a real
		// history is normal.
		for continuesLine(cmd) && i+1 < len(lines) {
			cmd = cmd[:len(cmd)-1]
			i++
			cmd = append(cmd, '\n')
			cmd = append(cmd, lines[i]...)
		}

		text := string(unmetafy(cmd))
		if strings.TrimSpace(text) == "" {
			continue
		}
		e := Entry{Command: text}
		if started > 0 {
			e.StartTime = started * 1000
			e.DurationMS = elapsed * 1000
		}
		out = append(out, e)
	}
	// The undated entries are the ones from before the switch, so they are the
	// oldest; approximateUndated places each run before the dated entry that
	// follows it rather than against the file's mtime.
	approximateUndated(out, modTime)
	return out
}

// zshMeta is the escape byte zsh uses inside its own strings.
const zshMeta = 0x83

// unmetafy reverses zsh's internal escaping, where a byte it treats as special
// is written as 0x83 followed by that byte XOR 0x20.
//
// Without this, any command containing an em dash or similar comes back as
// invalid UTF-8 — the bytes 0x80 to 0x9f are exactly the continuation bytes of
// common multi-byte characters, so the damage lands on ordinary punctuation
// pasted in from a browser.
func unmetafy(b []byte) []byte {
	// IndexByte, not ContainsRune: 0x83 as a rune is U+0083, which encodes as
	// two bytes, so a rune search never matches the raw escape byte at all.
	if bytes.IndexByte(b, zshMeta) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == zshMeta && i+1 < len(b) {
			i++
			out = append(out, b[i]^32)
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// ---------------------------------------------------------------- bash

func parseBash(raw []byte, modTime time.Time) []Entry {
	var out []Entry
	var pending int64 // a `#<epoch>` line applies to the entry after it

	lines := bytes.Split(raw, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// bash writes `#<epoch>` before each entry, but only when HISTTIMEFORMAT
		// was set. Most histories have none at all.
		if line[0] == '#' && allDigits(line[1:]) {
			if ts, err := strconv.ParseInt(string(line[1:]), 10, 64); err == nil {
				pending = ts
			}
			continue
		}
		cmd := append([]byte(nil), line...)
		// bash continues a multi-line entry with a trailing backslash.
		for continuesLine(cmd) && i+1 < len(lines) {
			cmd = cmd[:len(cmd)-1]
			i++
			cmd = append(cmd, '\n')
			cmd = append(cmd, lines[i]...)
		}
		if strings.TrimSpace(string(cmd)) == "" {
			continue
		}
		e := Entry{Command: string(cmd)}
		if pending > 0 {
			e.StartTime = pending * 1000
			pending = 0
		}
		out = append(out, e)
	}

	// An undated history still has an order, and order is most of what makes it
	// useful. Rather than stack everything on the epoch, entries are spaced one
	// second apart ending at the file's last modification — close enough to put
	// them in the right era, and flagged approximate so nothing pretends this was
	// measured.
	approximateUndated(out, modTime)
	return out
}

// approximateUndated dates the entries the file left undated, keeping them in
// the order they appear in.
//
// Each run of undated entries is placed between its dated neighbours rather
// than against the file's modification time. Using the modification time for
// all of them inverts the common history where HISTTIMEFORMAT was switched on
// partway through: the undated entries are the old ones at the top of the
// file, and stamping them near the mtime puts them after the dated entries
// below them — destroying exactly the order this is here to preserve.
func approximateUndated(entries []Entry, end time.Time) {
	for i := 0; i < len(entries); {
		if entries[i].StartTime != 0 {
			i++
			continue
		}
		j := i
		for j < len(entries) && entries[j].StartTime == 0 {
			j++
		}
		n := j - i

		// The run belongs strictly between the dated entry above it and the one
		// below, so spacing is divided to fit rather than fixed at a second.
		upper := end
		if j < len(entries) {
			upper = time.UnixMilli(entries[j].StartTime)
		}
		lower := upper.Add(-time.Duration(n+1) * time.Second)
		if i > 0 {
			if prev := time.UnixMilli(entries[i-1].StartTime); prev.After(lower) {
				lower = prev
			}
		}
		step := upper.Sub(lower) / time.Duration(n+1)

		for k := 0; k < n; k++ {
			entries[i+k].StartTime = lower.Add(time.Duration(k+1) * step).UnixMilli()
			entries[i+k].Approximate = true
		}
		i = j
	}
}

// continuesLine reports whether a history line ends in a backslash that escapes
// the newline, so the command carries on below.
//
// Counted rather than checked, because backslashes escape each other. `echo C:\\`
// ends in a literal backslash and is a whole command; reading it as a
// continuation swallowed whatever ran next — and under zsh it swallowed the
// following entry's `: 1700000001:0;` header along with it, storing the two as
// one unparseable command.
func continuesLine(s []byte) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// ---------------------------------------------------------------- fish

// parseFish reads fish's history, which looks like YAML but is not: values are
// written with fish's own escaping and can contain anything.
func parseFish(raw []byte) []Entry {
	var out []Entry
	var cur *Entry

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "- cmd: "):
			if cur != nil && strings.TrimSpace(cur.Command) != "" {
				out = append(out, *cur)
			}
			cur = &Entry{Command: unescapeFish(strings.TrimPrefix(line, "- cmd: "))}
		case strings.HasPrefix(line, "  when: ") && cur != nil:
			if ts, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "  when: ")), 10, 64); err == nil {
				cur.StartTime = ts * 1000
			}
		}
	}
	if cur != nil && strings.TrimSpace(cur.Command) != "" {
		out = append(out, *cur)
	}
	return out
}

// unescapeFish decodes the two escapes fish writes into a history value: \n
// for a newline and \\ for a literal backslash.
//
// One pass rather than two ReplaceAll calls. Replacing them one at a time has
// to handle \\ before \n, or `\\n` — an escaped backslash followed by the
// letter n — decodes as a newline instead of the two characters it is.
func unescapeFish(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case '\\':
			b.WriteByte('\\')
		default:
			// Anything else fish did not escape; keep it as written.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// String makes a Source printable in a report.
func (s *Source) String() string {
	return fmt.Sprintf("%s (%s, %d entries)", s.Path, s.Kind, len(s.Entries))
}
