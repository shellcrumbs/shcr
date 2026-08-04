// Package histimport brings a shell's own history file into the store.
//
// Separate from the command that calls it because the decisions here are worth
// testing: what an entry's identity is, whether it is already present, and what
// happens to one that carries a secret. Import is also the first thing most
// people run, on a file they cannot easily reproduce if it goes wrong.
package histimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/histfile"
	"github.com/shellcrumbs/shcr/internal/redact"
	"github.com/shellcrumbs/shcr/internal/store"
)

// Store is what an import needs from the database, and nothing else.
type Store interface {
	AppendEvent(ev event.Event) (bool, error)
	CommandByID(id string) (*store.Command, error)
}

// Result counts what one file contributed.
type Result struct {
	// New is entries that were not already here.
	New int
	// Existing is entries already present. Running an import twice should put
	// everything in this column and nothing in New.
	Existing int
	// Redacted is entries whose text had a secret replaced before storing.
	Redacted int
	// Skipped is entries dropped entirely, matching a skip rule.
	Skipped int
}

// Options configure one import.
type Options struct {
	DeviceID string
	Hostname string
	// Redactor is applied to every entry before anything is written. Old
	// history is exactly where credentials accumulate, so an import that
	// bypassed it would be the largest hole in the tool.
	Redactor *redact.Redactor
	// DryRun answers what would happen without writing. It has to reach the
	// database anyway: an entry's identity comes from its content, so whether
	// it is already present is knowable, and reporting everything as new made
	// the one number this flag exists to produce wrong on any re-import.
	DryRun bool
}

// File imports one parsed history file.
func File(st Store, src *histfile.Source, o Options) (Result, error) {
	var r Result
	if src == nil {
		return r, nil
	}
	for _, e := range src.Entries {
		text, action, _ := o.Redactor.Apply(e.Command)
		if action == redact.ActionSkip {
			r.Skipped++
			continue
		}
		if action == redact.ActionRedact {
			r.Redacted++
		}

		ev := Event(o.DeviceID, o.Hostname, string(src.Kind), src.Path, text, e)
		if o.DryRun {
			c, err := st.CommandByID(ev.CommandID)
			if err != nil {
				return r, fmt.Errorf("%s: %w", src.Path, err)
			}
			if c == nil {
				r.New++
			} else {
				r.Existing++
			}
			continue
		}
		inserted, err := st.AppendEvent(ev)
		if err != nil {
			return r, fmt.Errorf("%s: %w", src.Path, err)
		}
		if inserted {
			r.New++
		} else {
			r.Existing++
		}
	}
	return r, nil
}

// Event builds the event for one history entry.
//
// Its identity is derived from the content — shell, timestamp and text —
// because a history file is appended to and trimmed constantly, and anything
// keyed on position would duplicate the lot the first time the file rolled
// over. That is also what makes a second import of the same file add nothing.
func Event(deviceID, host, shell, source, text string, e histfile.Entry) event.Event {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"shcr-import-v1", shell, strconv.FormatInt(e.StartTime, 10), text,
	}, "\x00")))
	id := "import-" + hex.EncodeToString(sum[:12])

	payload, _ := json.Marshal(event.ImportPayload{
		Command:         text,
		Hostname:        host,
		Shell:           shell,
		StartTime:       e.StartTime,
		ApproximateTime: e.Approximate,
		DurationMS:      e.DurationMS,
		Source:          filepath.Base(source),
	})
	return event.Event{
		EventID:   id,
		CommandID: id,
		DeviceID:  deviceID,
		Type:      event.TypeImport,
		Payload:   payload,
		CreatedAt: e.StartTime,
	}
}

// Add totals one file's result into another.
func (r *Result) Add(other Result) {
	r.New += other.New
	r.Existing += other.Existing
	r.Redacted += other.Redacted
	r.Skipped += other.Skipped
}

// Detail is the one-line summary shown under each file.
func (r Result) Detail() string {
	out := fmt.Sprintf("%d new", r.New)
	if r.Existing > 0 {
		out += fmt.Sprintf(", %d already present", r.Existing)
	}
	if r.Redacted > 0 {
		out += fmt.Sprintf(", %d with secrets redacted", r.Redacted)
	}
	if r.Skipped > 0 {
		out += fmt.Sprintf(", %d skipped entirely", r.Skipped)
	}
	return out
}
