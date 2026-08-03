package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkEvent(t *testing.T, cmdID string, typ event.Type, payload any, createdAt int64) event.Event {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return event.Event{
		EventID:   fmt.Sprintf("%s-%s-%d", cmdID, typ, createdAt),
		CommandID: cmdID,
		DeviceID:  "device-a",
		Type:      typ,
		Payload:   b,
		CreatedAt: createdAt,
	}
}

func startEvent(t *testing.T, cmdID, text string, at int64) event.Event {
	return mkEvent(t, cmdID, event.TypeStart, event.StartPayload{
		Command:   text,
		Hostname:  "host-a",
		SessionID: "sess-1",
		Cwd:       "/home/u/app",
		Shell:     "bash",
		StartTime: at,
		PGID:      4242,
	}, at)
}

func endEvent(t *testing.T, cmdID string, exit int, at int64) event.Event {
	return mkEvent(t, cmdID, event.TypeEnd, event.EndPayload{EndTime: at, ExitCode: exit}, at)
}

func getOne(t *testing.T, s *Store, id string) Command {
	t.Helper()
	c, err := s.CommandByID(id)
	if err != nil {
		t.Fatalf("CommandByID: %v", err)
	}
	if c == nil {
		t.Fatalf("command %s not found", id)
	}
	return *c
}

func TestStartThenEnd(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendEvent(startEvent(t, "c1", "make build", 1000)); err != nil {
		t.Fatal(err)
	}

	c := getOne(t, s, "c1")
	if c.Status != StatusRunning {
		t.Fatalf("status = %q, want running", c.Status)
	}
	if c.EndTime != nil || c.ExitCode != nil {
		t.Fatal("running command should have no end time or exit code")
	}

	if _, err := s.AppendEvent(endEvent(t, "c1", 0, 1250)); err != nil {
		t.Fatal(err)
	}
	c = getOne(t, s, "c1")
	if c.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", c.Status)
	}
	if c.DurationMS == nil || *c.DurationMS != 250 {
		t.Fatalf("duration = %v, want 250", c.DurationMS)
	}
}

func TestNonZeroExitIsFailed(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "false", 1000))
	mustAppend(t, s, endEvent(t, "c1", 1, 1010))
	if c := getOne(t, s, "c1"); c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", c.Status)
	}
}

// Replaying an event must be a no-op — this is what makes sync merging safe.
func TestReplayIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	start := startEvent(t, "c1", "go test ./...", 1000)
	end := endEvent(t, "c1", 0, 2000)

	mustAppend(t, s, start)
	mustAppend(t, s, end)
	before := getOne(t, s, "c1")

	inserted, err := s.AppendEvent(start)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("re-appending a known event reported it as new")
	}
	mustAppendDup(t, s, end)

	if after := getOne(t, s, "c1"); snapshot(t, after) != snapshot(t, before) {
		t.Fatalf("state changed on replay:\n before %s\n after  %s", snapshot(t, before), snapshot(t, after))
	}

	var events int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE command_id='c1'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("event count = %d, want 2", events)
	}
}

// Sync will deliver `end` before `start`; the row must still converge.
func TestOutOfOrderConverges(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, endEvent(t, "c1", 3, 2000))

	c := getOne(t, s, "c1")
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed even without a start event", c.Status)
	}
	if c.Command != "" {
		t.Fatalf("command text = %q, want empty until start arrives", c.Command)
	}

	mustAppend(t, s, startEvent(t, "c1", "npm run build", 1000))
	c = getOne(t, s, "c1")
	if c.Command != "npm run build" {
		t.Fatalf("command = %q, want %q", c.Command, "npm run build")
	}
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", c.Status)
	}
	if c.DurationMS == nil || *c.DurationMS != 1000 {
		t.Fatalf("duration = %v, want 1000", c.DurationMS)
	}
}

// Any arrival order of the same event set must produce the same row.
func TestAllPermutationsConverge(t *testing.T) {
	events := []event.Event{
		startEvent(t, "c1", "terraform apply", 1000),
		endEvent(t, "c1", 0, 5000),
		mkEvent(t, "c1", event.TypeOrphan, map[string]any{"reason": "sweep"}, 4000),
	}

	var want Command
	var wantJSON string
	for i, perm := range permutations(events) {
		s := newTestStore(t)
		for _, ev := range perm {
			mustAppend(t, s, ev)
		}
		got := getOne(t, s, "c1")
		if i == 0 {
			want, wantJSON = got, snapshot(t, got)
			continue
		}
		if gotJSON := snapshot(t, got); gotJSON != wantJSON {
			t.Fatalf("permutation %d diverged:\n want %s\n got  %s", i, wantJSON, gotJSON)
		}
	}
	// An end event means the command reported a result, so it outranks the
	// sweep's guess that it was orphaned.
	if want.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (end beats orphan)", want.Status)
	}
}

func TestOrphanWithoutEnd(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "ssh prod", 1000))
	mustAppend(t, s, mkEvent(t, "c1", event.TypeOrphan, map[string]any{"reason": "sweep"}, 2000))

	c := getOne(t, s, "c1")
	if c.Status != StatusOrphaned {
		t.Fatalf("status = %q, want orphaned", c.Status)
	}
	if c.ExitCode != nil {
		t.Fatal("orphaned command must not carry an exit code")
	}
}

func TestRedactReplacesTextKeepsMetadata(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "export AWS_SECRET_ACCESS_KEY=hunter2", 1000))
	mustAppend(t, s, endEvent(t, "c1", 0, 1100))
	mustAppend(t, s, mkEvent(t, "c1", event.TypeRedact, map[string]any{}, 1200))

	c := getOne(t, s, "c1")
	if c.Command != event.RedactedMarker {
		t.Fatalf("command = %q, want %q", c.Command, event.RedactedMarker)
	}
	if c.Cwd != "/home/u/app" || c.Status != StatusCompleted {
		t.Fatalf("redaction should keep metadata, got %+v", c)
	}

	hits, err := s.QueryCommands(Filter{Text: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatal("redacted text is still reachable through search")
	}
}

func TestSearchAndFilters(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "npm run build:prod", 1000))
	mustAppend(t, s, endEvent(t, "c1", 0, 1500))
	mustAppend(t, s, startEvent(t, "c2", "git push --force-with-lease", 2000))
	mustAppend(t, s, endEvent(t, "c2", 1, 2500))
	mustAppend(t, s, startEvent(t, "c3", "npm test", 3000))

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"prefix match", Filter{Text: "npm"}, 2},
		{"multi token", Filter{Text: "npm run"}, 1},
		{"partial last token", Filter{Text: "npm bui"}, 1},
		{"punctuation is not fts syntax", Filter{Text: "--force-with-lease"}, 1},
		{"status", Filter{Status: StatusFailed}, 1},
		{"still running", Filter{Status: StatusRunning}, 1},
		{"time window", Filter{Since: 2000}, 2},
		{"no match", Filter{Text: "kubectl"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.QueryCommands(tc.filter)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d results, want %d", len(got), tc.want)
			}
		})
	}
}

func TestMultilineRoundTripsByteIdentical(t *testing.T) {
	s := newTestStore(t)
	const heredoc = "cat <<'EOF' > f.txt\n  line one\n\tline two\n\nEOF"
	mustAppend(t, s, startEvent(t, "c1", heredoc, 1000))
	if got := getOne(t, s, "c1").Command; got != heredoc {
		t.Fatalf("multiline command changed:\n want %q\n got  %q", heredoc, got)
	}
}

func TestUnsyncedEventLifecycle(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "echo hi", 1000))
	mustAppend(t, s, endEvent(t, "c1", 0, 1100))

	pending, err := s.UnsyncedEvents("device-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("unsynced = %d, want 2", len(pending))
	}
	ids := []string{pending[0].EventID}
	if err := s.MarkSynced(ids); err != nil {
		t.Fatal(err)
	}
	pending, err = s.UnsyncedEvents("device-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("unsynced after marking one = %d, want 1", len(pending))
	}
}

func TestLiveCommandsOnlyOwnDevice(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, startEvent(t, "c1", "sleep 100", 1000))

	peer := startEvent(t, "c2", "sleep 200", 1000)
	peer.DeviceID = "device-b"
	peer.EventID = "peer-start"
	mustAppend(t, s, peer)

	live, err := s.LiveCommands("device-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "c1" {
		t.Fatalf("live = %+v, want only c1 (peers must not be swept locally)", live)
	}
}

func TestQueryPerformanceAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	if raceEnabled {
		// The race detector adds an order of magnitude of overhead, so wall-clock
		// budgets say nothing about the code under it.
		t.Skip("timing budgets are not meaningful under -race")
	}
	s := newTestStore(t)

	// The budget: 500k rows, filtered queries under 50ms.
	const n = 500_000
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	cmdStmt, err := tx.Prepare(`INSERT INTO commands
		(id, command, hostname, device_id, session_id, cwd, shell, start_time,
		 end_time, exit_code, duration_ms, status, pgid, is_background)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0)`)
	if err != nil {
		t.Fatal(err)
	}
	verbs := []string{"git commit -m", "npm run build", "go test ./...", "kubectl get pods", "docker compose up"}
	for i := range n {
		text := fmt.Sprintf("%s %d", verbs[i%len(verbs)], i)
		start := int64(1_700_000_000_000 + i*1000)
		status := StatusCompleted
		if i%50 == 0 {
			status = StatusFailed
		}
		if _, err := cmdStmt.Exec(fmt.Sprintf("cmd-%d", i), text, "host-a", "device-a",
			"sess", "/home/u", "bash", start, start+42, 0, 42, status, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO commands_fts(rowid, command) SELECT rowid, command FROM commands`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		filter Filter
		budget time.Duration
	}{
		{"unfiltered recent", Filter{Limit: 50}, 50 * time.Millisecond},
		{"status", Filter{Status: StatusFailed, Limit: 50}, 50 * time.Millisecond},
		{"host", Filter{Hostname: "host-a", Limit: 50}, 50 * time.Millisecond},
		{"selective text", Filter{Text: "kubectl get pods 12345", Limit: 50}, 50 * time.Millisecond},
		{"no match", Filter{Text: "terraform", Limit: 50}, 50 * time.Millisecond},
		// A single very common token matches ~100k of the 500k rows, and the
		// exact answer means walking every one of them. Interactive search
		// avoids paying this by not querying until the user has typed enough to
		// be selective; the budget here just pins the worst case.
		{"broad single token", Filter{Text: "npm", Limit: 50}, 150 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			got, err := s.QueryCommands(tc.filter)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if elapsed := time.Since(started); elapsed > tc.budget {
				t.Errorf("took %v, budget is %v (%d rows)", elapsed, tc.budget, len(got))
			}
		})
	}
}

// snapshot renders a command for comparison; Command holds pointers, so
// comparing the structs directly would compare addresses.
func snapshot(t *testing.T, c Command) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustAppend(t *testing.T, s *Store, ev event.Event) {
	t.Helper()
	inserted, err := s.AppendEvent(ev)
	if err != nil {
		t.Fatalf("append %s: %v", ev.Type, err)
	}
	if !inserted {
		t.Fatalf("append %s: expected a new event", ev.Type)
	}
}

func mustAppendDup(t *testing.T, s *Store, ev event.Event) {
	t.Helper()
	if _, err := s.AppendEvent(ev); err != nil {
		t.Fatalf("re-append %s: %v", ev.Type, err)
	}
}

func permutations(in []event.Event) [][]event.Event {
	if len(in) <= 1 {
		return [][]event.Event{append([]event.Event(nil), in...)}
	}
	var out [][]event.Event
	for i := range in {
		rest := make([]event.Event, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]event.Event{in[i]}, p...))
		}
	}
	return out
}

// SQLite creates its files 0644. The containing directory is 0700, but that is
// a single line of defence: a copy, a backup, or a directory whose mode drifted
// would expose the whole history to anyone with a login on the machine. The
// write-ahead log holds the most recent commands and matters just as much.
func TestDatabaseFilesAreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Force the WAL and shared-memory files into existence.
	mustAppend(t, s, startEvent(t, "c1", "echo hi", 1000))

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		fi, err := os.Stat(p)
		if err != nil {
			continue // -shm only appears once a reader maps it
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %04o; group or other can read the command history", filepath.Base(p), perm)
		}
	}
}

// Export tells the user that `--events` is "the only form that can rebuild this
// database". That is a promise about recovery, so it is worth proving rather
// than asserting in a help string.
func TestExportedEventsRebuildTheDatabase(t *testing.T) {
	original := newTestStore(t)
	mustAppend(t, original, startEvent(t, "c1", "npm run build", 1000))
	mustAppend(t, original, endEvent(t, "c1", 1, 1500))
	mustAppend(t, original, startEvent(t, "c2", "ssh prod", 2000))
	mustAppend(t, original, mkEvent(t, "c2", event.TypeOrphan, map[string]any{"reason": "sweep"}, 3000))
	mustAppend(t, original, startEvent(t, "c3", "export TOKEN=hunter2", 4000))
	mustAppend(t, original, endEvent(t, "c3", 0, 4100))
	mustAppend(t, original, mkEvent(t, "c3", event.TypeRedact, map[string]any{}, 4200))
	mustAppend(t, original, mkEvent(t, "c4", event.TypeImport, event.ImportPayload{
		Command: "make release", Hostname: "h", Shell: "zsh", StartTime: 5000, Source: ".zsh_history",
	}, 5000))

	// Export the log the way `shcr export --events` does.
	var exported []event.Event
	if err := original.EachEvent(func(ev event.Event) error {
		exported = append(exported, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(exported) != 8 {
		t.Fatalf("exported %d events, want 8", len(exported))
	}

	// Replay into an empty database, as a restore would.
	restored := newTestStore(t)
	for _, ev := range exported {
		if _, err := restored.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
	}

	before, err := original.QueryCommands(Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	after, err := restored.QueryCommands(Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("rebuilt %d commands from %d", len(after), len(before))
	}
	for i := range before {
		if snapshot(t, before[i]) != snapshot(t, after[i]) {
			t.Errorf("command %d differs after rebuild:\n before %s\n after  %s",
				i, snapshot(t, before[i]), snapshot(t, after[i]))
		}
	}
	// Derived state has to survive too, not just the rows.
	hits, err := restored.QueryCommands(Filter{Text: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Error("a redaction did not survive the rebuild: the secret is searchable again")
	}
}

// Export must select exactly what `list` shows, or the two drift.
func TestStreamedExportMatchesTheQueryItMirrors(t *testing.T) {
	s := newTestStore(t)
	for i, text := range []string{"npm run build", "npm test", "git push", "make"} {
		at := int64(1000 + i*100)
		mustAppend(t, s, startEvent(t, fmt.Sprintf("c%d", i), text, at))
		exit := 0
		if i == 2 {
			exit = 1
		}
		mustAppend(t, s, endEvent(t, fmt.Sprintf("c%d", i), exit, at+50))
	}

	for _, f := range []Filter{
		{},
		{Text: "npm"},
		{Status: StatusFailed},
		{Since: 1200},
	} {
		want, err := s.QueryCommands(Filter{
			Text: f.Text, Status: f.Status, Hostname: f.Hostname, Since: f.Since, Limit: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []Command
		if err := s.EachCommand(f, func(c Command) error {
			got = append(got, c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Errorf("filter %+v: streamed %d, query returned %d", f, len(got), len(want))
		}
	}
}

// Streaming means never holding the result set, so there is no limit to forget.
func TestEachCommandHasNoImplicitLimit(t *testing.T) {
	s := newTestStore(t)
	const n = 250 // above QueryCommands' default of 100
	for i := range n {
		mustAppend(t, s, startEvent(t, fmt.Sprintf("c%d", i), fmt.Sprintf("cmd %d", i), int64(1000+i)))
	}
	count := 0
	if err := s.EachCommand(Filter{}, func(Command) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("streamed %d of %d commands — a default limit leaked in", count, n)
	}
	// And it streams oldest first, so an export reads like a log.
	var first Command
	_ = s.EachCommand(Filter{}, func(c Command) error {
		if first.ID == "" {
			first = c
		}
		return nil
	})
	if first.StartTime != 1000 {
		t.Errorf("export should start at the oldest command, got start_time %d", first.StartTime)
	}
}

// A callback that fails must stop the scan rather than be ignored.
func TestEachCommandPropagatesCallbackErrors(t *testing.T) {
	s := newTestStore(t)
	for i := range 10 {
		mustAppend(t, s, startEvent(t, fmt.Sprintf("c%d", i), "x", int64(1000+i)))
	}
	sentinel := fmt.Errorf("disk full")
	seen := 0
	err := s.EachCommand(Filter{}, func(Command) error {
		seen++
		if seen == 3 {
			return sentinel
		}
		return nil
	})
	if err != sentinel {
		t.Fatalf("got %v, want the callback's error", err)
	}
	if seen != 3 {
		t.Errorf("kept going after the callback failed: %d rows", seen)
	}
}
