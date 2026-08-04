// Package export writes history out in the formats a person might want it in.
//
// It lives apart from the command that calls it so that it can be tested. A
// tool that takes custody of years of someone's shell history owes them a
// supported way out, and "supported" has to mean something more than that the
// author ran it once.
package export

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

// Format is how the history is written.
type Format string

const (
	// JSONL is one object per line: the form that streams, and the only one
	// that can carry the event log.
	JSONL Format = "jsonl"
	// JSON is a single array, still written element by element rather than
	// assembled in memory.
	JSON Format = "json"
	// CSV is for spreadsheets, and therefore for commands only — the event log
	// has a nested payload that a flat row cannot hold.
	CSV Format = "csv"
)

// ParseFormat accepts the names the flag advertises.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "", JSONL:
		return JSONL, nil
	case JSON, CSV:
		return f, nil
	default:
		return "", fmt.Errorf("unknown format %q (want jsonl, json or csv)", s)
	}
}

// Source is the part of the store an export reads. An interface so the
// encoders can be tested without a database, and so that neither of them can
// quietly start reaching for anything else.
type Source interface {
	EachCommand(f store.Filter, fn func(store.Command) error) error
	EachEvent(fn func(event.Event) error) error
}

// Options is what to write and in which shape.
type Options struct {
	Format Format
	// Events writes the raw log instead of command rows. It is the lossless
	// form: command rows are derived from these, so replaying them into an
	// empty database reconstructs everything, redactions included.
	Events bool
	Filter store.Filter
}

// Write streams the history to w and reports how many records it wrote.
//
// Streaming rather than buffering is the point: half a million commands go out
// in about the memory ten thousand do.
func Write(w io.Writer, src Source, o Options) (int, error) {
	buf := bufio.NewWriterSize(w, 128*1024)
	n := 0
	var err error

	switch o.Format {
	case JSON:
		if _, err = buf.WriteString("[\n"); err != nil {
			return 0, err
		}
		enc := json.NewEncoder(buf)
		emit := func(v any) error {
			if n > 0 {
				if _, err := buf.WriteString(","); err != nil {
					return err
				}
			}
			n++
			return enc.Encode(v)
		}
		if o.Events {
			err = src.EachEvent(func(ev event.Event) error { return emit(ev) })
		} else {
			err = src.EachCommand(o.Filter, func(c store.Command) error { return emit(c) })
		}
		if err == nil {
			_, err = buf.WriteString("]\n")
		}

	case CSV:
		if o.Events {
			return 0, fmt.Errorf("the event log has a nested payload; use --format jsonl for --events")
		}
		cw := csv.NewWriter(buf)
		if err = cw.Write(csvHeader); err != nil {
			return 0, err
		}
		err = src.EachCommand(o.Filter, func(c store.Command) error {
			n++
			return cw.Write(csvRow(c))
		})
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}

	default: // JSONL
		enc := json.NewEncoder(buf)
		if o.Events {
			err = src.EachEvent(func(ev event.Event) error { n++; return enc.Encode(ev) })
		} else {
			err = src.EachCommand(o.Filter, func(c store.Command) error { n++; return enc.Encode(c) })
		}
	}

	if err != nil {
		return n, err
	}
	return n, buf.Flush()
}

var csvHeader = []string{
	"id", "started", "command", "host", "cwd", "branch", "shell",
	"status", "exit_code", "duration_ms", "session", "imported",
}

func csvRow(c store.Command) []string {
	return []string{
		c.ID,
		// RFC3339 rather than the stored milliseconds: a spreadsheet can sort
		// and filter a date, and nobody reading one can do anything with
		// 1770315592000.
		time.UnixMilli(c.StartTime).Format(time.RFC3339),
		c.Command,
		c.Hostname,
		c.Cwd,
		derefString(c.GitBranch),
		c.Shell,
		c.Status,
		derefInt(c.ExitCode),
		derefInt64(c.DurationMS),
		c.SessionID,
		strconv.FormatBool(c.Imported),
	}
}

// An absent value is an empty cell rather than a zero: a command with no exit
// code has not exited zero, and a spreadsheet cannot tell the difference once
// it has been written as one.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func derefInt64(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}
