package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/paths"
	"github.com/shellcrumbs/shcr/internal/store"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)

	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, "dev-test", nil, nil)
}

func spoolLine(t *testing.T, id string) []byte {
	t.Helper()
	payload, err := json.Marshal(event.StartPayload{
		Command: "echo " + id, Hostname: "h", SessionID: "s", Cwd: "/tmp", Shell: "bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(event.Event{
		EventID: id, CommandID: id, DeviceID: "dev-test",
		Type: event.TypeStart, Payload: payload, CreatedAt: 1_700_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// A daemon that dies partway through a drain leaves the spool renamed to
// .draining. The hooks have already moved on to a fresh spool, so nothing else
// will ever mention that file: if the next startup does not read it, those
// events are lost — exactly the failure the spool exists to prevent.
func TestDrainRecoversASpoolLeftByACrash(t *testing.T) {
	d := testDaemon(t)
	if err := os.WriteFile(paths.SpoolPath()+".draining", spoolLine(t, "crashed"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := d.DrainSpool()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("drained %d events, want 1", n)
	}
	if c, err := d.store.CommandByID("crashed"); err != nil || c == nil {
		t.Errorf("the event from the abandoned file was not replayed (err %v)", err)
	}
	if _, err := os.Stat(paths.SpoolPath() + ".draining"); !os.IsNotExist(err) {
		t.Error("the abandoned file should be removed once replayed")
	}
}

func TestDrainTakesBothTheAbandonedAndTheCurrentSpool(t *testing.T) {
	d := testDaemon(t)
	if err := os.WriteFile(paths.SpoolPath()+".draining", spoolLine(t, "old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.SpoolPath(), spoolLine(t, "new"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := d.DrainSpool()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("drained %d events, want 2", n)
	}
	for _, id := range []string{"old", "new"} {
		if c, err := d.store.CommandByID(id); err != nil || c == nil {
			t.Errorf("event %q was not replayed (err %v)", id, err)
		}
	}
	if _, err := os.Stat(paths.SpoolPath()); !os.IsNotExist(err) {
		t.Error("the spool should be consumed")
	}
}

func TestDrainWithNothingToDoIsNotAnError(t *testing.T) {
	d := testDaemon(t)
	n, err := d.DrainSpool()
	if err != nil || n != 0 {
		t.Errorf("empty drain returned (%d, %v), want (0, nil)", n, err)
	}
}
