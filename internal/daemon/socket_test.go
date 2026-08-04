package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/paths"
	"github.com/shellcrumbs/shcr/internal/store"
)

// serve starts a daemon and waits until the socket actually answers, so no test
// below has to guess how long startup takes. It returns a dial function and
// stops the daemon on cleanup.
func serve(t *testing.T, d *Daemon) func(*testing.T) net.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			// A cancelled daemon is a normal exit, not a failure: it is what
			// happens on every logout.
			if err != nil {
				t.Errorf("daemon returned %v on shutdown, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down when its context was cancelled")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", d.sockPath)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never started listening on %s: %v", d.sockPath, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	return func(t *testing.T) net.Conn {
		t.Helper()
		c, err := net.Dial("unix", d.sockPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
}

func startLine(t *testing.T, id, command string) []byte {
	t.Helper()
	payload, err := json.Marshal(event.StartPayload{
		Command: command, Hostname: "laptop", SessionID: "s1", Cwd: "/home/u",
		Shell: "bash", StartTime: event.NowMillis(), PGID: os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(event.Event{
		EventID: id + "-start", CommandID: id, DeviceID: "dev-test",
		Type: event.TypeStart, Payload: payload, CreatedAt: event.NowMillis(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// waitForCommand gives the daemon a moment to process what was just written.
// The socket write returns before the event has been through ingest.
func waitForCommand(t *testing.T, d *Daemon, id string) *store.Command {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := d.store.CommandByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if c != nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("command %s never reached the store", id)
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForStatus polls until a command reaches the expected status. Run starts
// listening before it drains and sweeps, so a dialable socket does not mean
// startup has finished — reading the status straight after connecting is a race,
// and one that only shows up on a loaded machine.
func waitForStatus(t *testing.T, d *Daemon, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = statusOf(t, d, id); got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("command %s settled at %q, want %q", id, got, want)
}

// The path every recorded command takes: hook writes a line to the socket, the
// daemon stores it.
func TestAnEventWrittenToTheSocketIsStored(t *testing.T) {
	d := testDaemon(t)
	dial := serve(t, d)

	conn := dial(t)
	if _, err := conn.Write(startLine(t, "c1", "npm run build")); err != nil {
		t.Fatal(err)
	}

	c := waitForCommand(t, d, "c1")
	if c.Command != "npm run build" || c.Status != store.StatusRunning {
		t.Errorf("stored %+v", c)
	}
}

// One shell sending nonsense — an old binary, a hand-written hook, a truncated
// write — must not take down the connection the rest of the session depends on.
func TestOneBadLineDoesNotStopTheRest(t *testing.T) {
	d := testDaemon(t)
	dial := serve(t, d)
	conn := dial(t)

	for _, line := range [][]byte{
		[]byte("{ this is not json\n"),
		[]byte("{\"event_id\":\"x\"}\n"), // valid json, missing required fields
		[]byte("\n"),                     // an empty line
		startLine(t, "after", "echo still here"),
	} {
		if _, err := conn.Write(line); err != nil {
			t.Fatal(err)
		}
	}

	if c := waitForCommand(t, d, "after"); c.Command != "echo still here" {
		t.Errorf("stored %+v", c)
	}
}

// The socket carries two shapes. A nudge is a request to sync, not something to
// record — storing them would put junk rows in everyone's history.
func TestANudgeTriggersASyncAndIsNotStored(t *testing.T) {
	d := testDaemon(t)
	got := make(chan string, 4)
	d.OnTrigger = func(reason string) { got <- reason }
	dial := serve(t, d)

	conn := dial(t)
	if _, err := conn.Write([]byte(`{"nudge":"shell-start"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case reason := <-got:
		if reason != "shell-start" {
			t.Errorf("triggered on %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a nudge did not trigger a sync")
	}

	var events int
	if err := d.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("a nudge left %d event(s) in the log", events)
	}
}

// A machine that lost power leaves the socket file behind. Refusing to start
// until someone deletes it by hand would strand history on every unclean reboot.
func TestAStaleSocketFromACrashedDaemonIsCleared(t *testing.T) {
	d := testDaemon(t)
	if err := os.MkdirAll(filepath.Dir(d.sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// A plain file at the socket path: what is left when nothing is listening.
	if err := os.WriteFile(d.sockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	dial := serve(t, d)
	conn := dial(t)
	if _, err := conn.Write(startLine(t, "c1", "echo recovered")); err != nil {
		t.Fatal(err)
	}
	if c := waitForCommand(t, d, "c1"); c.Command != "echo recovered" {
		t.Errorf("stored %+v", c)
	}
}

// The other side of that: a socket a live daemon is serving must never be
// removed. Two daemons on one database would each hold half the session's
// events, and whichever started last would silently take the shells with it.
func TestALiveDaemonsSocketIsNotStolen(t *testing.T) {
	first := testDaemon(t)
	serve(t, first)

	// A second daemon, same user, same socket path — `shcr daemon` run twice.
	st, err := store.Open(filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	second := New(st, "dev-test", nil, nil)

	// Bounded: a second daemon that wrongly got the socket would otherwise sit
	// in Accept forever and hang the test rather than failing it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = second.Run(ctx)
	if err == nil {
		t.Fatal("a second daemon took over the socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("the error should say what is wrong: %v", err)
	}

	// And the first is still the one serving.
	conn, err := net.Dial("unix", first.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(startLine(t, "c1", "echo first")); err != nil {
		t.Fatal(err)
	}
	if c := waitForCommand(t, first, "c1"); c.Command != "echo first" {
		t.Errorf("the first daemon stopped serving: %+v", c)
	}
}

// Everything anyone has ever typed goes through this socket, and on a shared
// machine /run is readable. Anyone who can open it can both read nothing and
// inject anything, so it belongs to one user.
func TestTheSocketIsNotReachableByOtherUsers(t *testing.T) {
	d := testDaemon(t)
	serve(t, d)

	fi, err := os.Stat(d.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode is %04o, want 0600", perm)
	}
}

// With no runtime directory the socket falls back to a path shcr chooses, and
// so has to create. That one is ours to get right — /run/user/$UID is already
// private and made by something else.
func TestTheFallbackSocketDirectoryIsPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	st, err := store.Open(filepath.Join(dir, "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := New(st, "dev-test", nil, nil)

	if got := filepath.Dir(d.sockPath); got != filepath.Join(dir, "shcr") {
		t.Fatalf("socket landed in %s, not the state directory", got)
	}
	serve(t, d)

	fi, err := os.Stat(filepath.Dir(d.sockPath))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("shcr created the socket's directory as %04o, which lets others in", perm)
	}
}

// Startup is where a crashed daemon's leftovers are settled: whatever the hooks
// spooled while it was down, and whatever was still running when it died.
func TestStartupDrainsTheSpoolAndSweeps(t *testing.T) {
	d := testDaemon(t)

	// A command left running by the previous daemon, under a shell that is gone.
	startCommand(t, d, "left-running", d.deviceID, deadPID(t), longAgo())
	// And an event the hooks spooled because there was nothing listening.
	if err := os.WriteFile(paths.SpoolPath(), startLine(t, "spooled", "echo spooled"), 0o600); err != nil {
		t.Fatal(err)
	}

	serve(t, d)

	if c := waitForCommand(t, d, "spooled"); c.Command != "echo spooled" {
		t.Errorf("the spool was not replayed: %+v", c)
	}
	waitForStatus(t, d, "left-running", store.StatusOrphaned)
	if _, err := os.Stat(paths.SpoolPath()); !os.IsNotExist(err) {
		t.Error("the spool file survived being drained; it would replay forever")
	}
}
