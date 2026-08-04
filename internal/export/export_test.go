package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

type fake struct {
	commands []store.Command
	events   []event.Event
	filter   store.Filter // what the last EachCommand was asked for
}

func (f *fake) EachCommand(flt store.Filter, fn func(store.Command) error) error {
	f.filter = flt
	for _, c := range f.commands {
		if err := fn(c); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) EachEvent(fn func(event.Event) error) error {
	for _, e := range f.events {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func sample() *fake {
	branch, exit, dur := "main", 1, int64(2400)
	return &fake{
		commands: []store.Command{
			{ID: "c1", Command: "npm run build", Hostname: "laptop", Cwd: "/home/u/app",
				GitBranch: &branch, Shell: "bash", StartTime: 1_700_000_000_000,
				ExitCode: &exit, DurationMS: &dur, Status: store.StatusFailed, SessionID: "s1"},
			// No exit code, no duration, no branch: still running.
			{ID: "c2", Command: `echo "he said, ""hi""" | tee /tmp/x`, Hostname: "server",
				Cwd: "/tmp", Shell: "zsh", StartTime: 1_700_000_001_000,
				Status: store.StatusRunning, SessionID: "s2", Imported: true},
		},
		events: []event.Event{
			{EventID: "e1", CommandID: "c1", DeviceID: "d1", Type: event.TypeStart,
				Payload: []byte(`{"command":"npm run build"}`), CreatedAt: 1_700_000_000_000},
		},
	}
}

func TestJSONLIsOneRecordPerLine(t *testing.T) {
	var buf bytes.Buffer
	n, err := Write(&buf, sample(), Options{Format: JSONL})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if n != 2 || len(lines) != 2 {
		t.Fatalf("wrote %d records over %d lines, want 2 and 2", n, len(lines))
	}
	for i, line := range lines {
		var c store.Command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Errorf("line %d is not a JSON object: %v", i, err)
		}
	}
}

func TestJSONIsOneValidArray(t *testing.T) {
	var buf bytes.Buffer
	n, err := Write(&buf, sample(), Options{Format: JSON})
	if err != nil {
		t.Fatal(err)
	}
	var got []store.Command
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, buf.String())
	}
	if n != 2 || len(got) != 2 {
		t.Fatalf("wrote %d records, decoded %d, want 2", n, len(got))
	}
	if got[0].Command != "npm run build" {
		t.Errorf("first record is %q", got[0].Command)
	}
}

// An empty result still has to be a parseable array rather than a stray comma
// or nothing at all.
func TestJSONWithNoRecordsIsStillAnArray(t *testing.T) {
	var buf bytes.Buffer
	n, err := Write(&buf, &fake{}, Options{Format: JSON})
	if err != nil {
		t.Fatal(err)
	}
	var got []store.Command
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty export is not valid JSON: %v (%q)", err, buf.String())
	}
	if n != 0 || len(got) != 0 {
		t.Errorf("n=%d, decoded %d", n, len(got))
	}
}

func TestCSVQuotesAndLeavesAbsentValuesEmpty(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Write(&buf, sample(), Options{Format: CSV}); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\n%s", err, buf.String())
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two records", len(rows))
	}
	if rows[0][0] != "id" || rows[0][8] != "exit_code" {
		t.Errorf("header is %v", rows[0])
	}
	// A command containing quotes, a comma and a pipe must survive the trip.
	if want := `echo "he said, ""hi""" | tee /tmp/x`; rows[2][2] != want {
		t.Errorf("command mangled:\n got  %q\n want %q", rows[2][2], want)
	}
	// A running command has not exited zero, and a spreadsheet cannot tell the
	// difference once an absent value has been written as one.
	if rows[2][8] != "" || rows[2][9] != "" {
		t.Errorf("absent exit code/duration became %q/%q, want empty", rows[2][8], rows[2][9])
	}
	if rows[1][8] != "1" || rows[1][5] != "main" {
		t.Errorf("present values lost: %v", rows[1])
	}
	// A date, not the stored milliseconds. A spreadsheet can sort and filter
	// the first; nobody can do anything with the second.
	if _, err := time.Parse(time.RFC3339, rows[1][1]); err != nil {
		t.Errorf("started column is not a date: %q", rows[1][1])
	}
}

// The event log is the lossless form, and its payload is nested. A flat row
// cannot hold it, so the export says so rather than writing something that
// looks complete.
func TestCSVRefusesTheEventLog(t *testing.T) {
	var buf bytes.Buffer
	_, err := Write(&buf, sample(), Options{Format: CSV, Events: true})
	if err == nil {
		t.Fatal("CSV accepted the event log")
	}
	if !strings.Contains(err.Error(), "jsonl") {
		t.Errorf("the error should point at the format that works: %v", err)
	}
}

func TestEventsExportTheLogNotTheRows(t *testing.T) {
	var buf bytes.Buffer
	n, err := Write(&buf, sample(), Options{Format: JSONL, Events: true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("wrote %d records, want the one event", n)
	}
	var ev event.Event
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "e1" || ev.Type != event.TypeStart {
		t.Errorf("got %+v", ev)
	}
}

func TestFilterReachesTheStore(t *testing.T) {
	src := sample()
	want := store.Filter{Text: "npm", Status: store.StatusFailed, Hostname: "laptop", Since: 42}
	if _, err := Write(&bytes.Buffer{}, src, Options{Format: JSONL, Filter: want}); err != nil {
		t.Fatal(err)
	}
	if src.filter != want {
		t.Errorf("store was asked for %+v, want %+v", src.filter, want)
	}
}

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Format
		ok   bool
	}{
		{"", JSONL, true}, {"jsonl", JSONL, true}, {"JSON", JSON, true},
		{" csv ", CSV, true}, {"yaml", "", false},
	} {
		got, err := ParseFormat(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("ParseFormat(%q) = %q, %v", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseFormat(%q) should have failed", tc.in)
		}
	}
}
