package histimport

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/histfile"
	"github.com/shellcrumbs/shcr/internal/redact"
	"github.com/shellcrumbs/shcr/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func source(kind histfile.Kind, path string, entries ...histfile.Entry) *histfile.Source {
	return &histfile.Source{Kind: kind, Path: path, Entries: entries}
}

func opts(dry bool) Options {
	return Options{DeviceID: "dev-1", Hostname: "laptop", Redactor: redact.New(nil), DryRun: dry}
}

// Running an import twice must add nothing the second time. A history file is
// appended to and trimmed constantly, so anyone who imports at all will import
// again.
func TestSecondImportAddsNothing(t *testing.T) {
	st := testStore(t)
	src := source(histfile.Zsh, "/home/u/.zsh_history",
		histfile.Entry{Command: "git status", StartTime: 1_700_000_000_000},
		histfile.Entry{Command: "npm run build", StartTime: 1_700_000_001_000},
	)

	first, err := File(st, src, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if first.New != 2 || first.Existing != 0 {
		t.Fatalf("first import: %+v", first)
	}

	second, err := File(st, src, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if second.New != 0 || second.Existing != 2 {
		t.Errorf("second import: %+v, want everything already present", second)
	}
}

// The count is the only thing --dry-run produces, so it has to be the count a
// real import would produce.
func TestDryRunCountsWhatARealImportWould(t *testing.T) {
	st := testStore(t)
	src := source(histfile.Bash, "/home/u/.bash_history",
		histfile.Entry{Command: "ls -al", StartTime: 1_700_000_000_000},
		histfile.Entry{Command: "make", StartTime: 1_700_000_001_000},
		histfile.Entry{Command: "vim", StartTime: 1_700_000_002_000},
	)

	dry, err := File(st, src, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	real, err := File(st, src, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if dry.New != real.New || dry.Existing != real.Existing {
		t.Errorf("dry run said %+v, the import did %+v", dry, real)
	}

	// And on a second pass both must agree that there is nothing to do.
	dryAgain, err := File(st, src, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	if dryAgain.New != 0 || dryAgain.Existing != 3 {
		t.Errorf("dry run over an imported file: %+v, want 0 new", dryAgain)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	st := testStore(t)
	src := source(histfile.Bash, "/home/u/.bash_history",
		histfile.Entry{Command: "echo hello", StartTime: 1_700_000_000_000})

	if _, err := File(st, src, opts(true)); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("a dry run wrote %d event(s)", events)
	}
}

// Old history is exactly where credentials accumulate. An import that bypassed
// redaction would be the largest hole in the tool.
func TestSecretsAreRedactedOnTheWayIn(t *testing.T) {
	st := testStore(t)
	src := source(histfile.Bash, "/home/u/.bash_history",
		histfile.Entry{Command: "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", StartTime: 1},
		histfile.Entry{Command: "git status", StartTime: 2},
	)

	r, err := File(st, src, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if r.Redacted != 1 {
		t.Errorf("redacted %d entries, want 1: %+v", r.Redacted, r)
	}

	var stored string
	if err := st.DB().QueryRow(
		`SELECT command FROM commands WHERE command LIKE '%AWS_SECRET%'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "wJalrXUtnFEMIK") {
		t.Errorf("the secret reached the database: %q", stored)
	}
	// Searching for it must not find it either.
	hits, err := st.QueryCommands(store.Filter{Text: "wJalrXUtnFEMIK"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("the secret is reachable through search")
	}
}

func TestSkippedEntriesAreNotStoredAtAll(t *testing.T) {
	st := testStore(t)
	red := redact.New([]redact.Rule{{
		Name:   "vault write",
		Re:     regexp.MustCompile(`^vault write`),
		Action: redact.ActionSkip,
	}})
	src := source(histfile.Bash, "/home/u/.bash_history",
		histfile.Entry{Command: "vault write secret/x value=y", StartTime: 1},
		histfile.Entry{Command: "ls", StartTime: 2},
	)

	o := opts(false)
	o.Redactor = red
	r, err := File(st, src, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 1 || r.New != 1 {
		t.Errorf("%+v, want one skipped and one stored", r)
	}
	rows, err := st.QueryCommands(store.Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Command != "ls" {
		t.Errorf("a skipped entry was stored: %+v", rows)
	}
}

// Identity comes from the content, so the same command at a different time is
// a different entry — and the same entry read from a differently named file is
// not.
func TestIdentityFollowsContentNotPosition(t *testing.T) {
	e := histfile.Entry{Command: "make deploy", StartTime: 1_700_000_000_000}
	base := Event("dev-1", "laptop", "bash", "/home/u/.bash_history", "make deploy", e)

	same := Event("dev-1", "laptop", "bash", "/home/u/backup/.bash_history", "make deploy", e)
	if same.CommandID != base.CommandID {
		t.Error("the same entry from a moved file counted as new")
	}

	later := e
	later.StartTime += 1000
	if Event("dev-1", "laptop", "bash", "/home/u/.bash_history", "make deploy", later).CommandID == base.CommandID {
		t.Error("the same command at a different time collapsed into one entry")
	}
	if Event("dev-1", "laptop", "zsh", "/home/u/.bash_history", "make deploy", e).CommandID == base.CommandID {
		t.Error("the same text from a different shell collapsed into one entry")
	}
	if Event("dev-1", "laptop", "bash", "/home/u/.bash_history", "make deployy", e).CommandID == base.CommandID {
		t.Error("different text produced the same identity")
	}
}

// No shell history format records an exit code, and most record no time. The
// payload has to say so rather than claim a result the file cannot support.
func TestImportedEntriesClaimNoResult(t *testing.T) {
	e := histfile.Entry{Command: "ls", StartTime: 1_700_000_000_000, Approximate: true}
	ev := Event("dev-1", "laptop", "bash", "/home/u/.bash_history", "ls", e)

	if ev.Type != event.TypeImport {
		t.Errorf("type = %q", ev.Type)
	}
	var p event.ImportPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if !p.ApproximateTime {
		t.Error("an approximate time must be flagged as one")
	}
	if p.Source != ".bash_history" {
		t.Errorf("source = %q, want the file's name and not its path", p.Source)
	}
	if strings.Contains(string(ev.Payload), "exit_code") {
		t.Error("an imported entry must not carry an exit code")
	}
}

func TestDetailNamesOnlyWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		r    Result
		want string
	}{
		{Result{New: 3}, "3 new"},
		{Result{New: 0, Existing: 12}, "0 new, 12 already present"},
		{Result{New: 1, Redacted: 2, Skipped: 1},
			"1 new, 2 with secrets redacted, 1 skipped entirely"},
	} {
		if got := tc.r.Detail(); got != tc.want {
			t.Errorf("%+v -> %q, want %q", tc.r, got, tc.want)
		}
	}
}
