package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

// deadPID returns a pid that certainly refers to nothing. The process is waited
// on before the pid is handed back, so it is reaped rather than a zombie — a
// zombie still answers kill(pid, 0) and would make every test here pass for the
// wrong reason.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("pid %d is still answering signals", pid)
	}
	return pid
}

// startCommand records a command that began and never ended, the way a shell
// hook would.
func startCommand(t *testing.T, d *Daemon, id, deviceID string, pgid int, at int64) {
	t.Helper()
	payload, err := json.Marshal(event.StartPayload{
		Command: "sleep 100", Hostname: "laptop", SessionID: "s1", Cwd: "/home/u",
		Shell: "bash", StartTime: at, PGID: pgid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.AppendEvent(event.Event{
		EventID: id + "-start", CommandID: id, DeviceID: deviceID,
		Type: event.TypeStart, Payload: payload, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func statusOf(t *testing.T, d *Daemon, id string) string {
	t.Helper()
	c, err := d.store.CommandByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatalf("command %s is not in the store", id)
	}
	return c.Status
}

// long ago, relative to now — comfortably outside the grace period.
func longAgo() int64 { return event.NowMillis() - int64(time.Hour/time.Millisecond) }

// The behaviour the tool is pitched on. A command whose shell is gone can never
// receive an end event, so its outcome is unknowable — and saying so is the
// point. Left alone it would read as still running forever.
func TestACommandWhoseShellIsGoneBecomesOrphaned(t *testing.T) {
	d := testDaemon(t)
	startCommand(t, d, "c1", d.deviceID, deadPID(t), longAgo())

	if got := statusOf(t, d, "c1"); got != store.StatusRunning {
		t.Fatalf("before the sweep the command is %q, want running", got)
	}

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d command(s), want 1", n)
	}
	if got := statusOf(t, d, "c1"); got != store.StatusOrphaned {
		t.Errorf("status is %q, want orphaned", got)
	}
}

// The other half of the same claim: a command still running under a live shell
// must be left alone. A sweep that orphaned these would be worse than no sweep.
func TestALiveShellIsLeftRunning(t *testing.T) {
	d := testDaemon(t)
	// This test's own process is beyond doubt alive.
	startCommand(t, d, "c1", d.deviceID, os.Getpid(), longAgo())

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("swept %d command(s) under a live shell, want 0", n)
	}
	if got := statusOf(t, d, "c1"); got != store.StatusRunning {
		t.Errorf("status is %q, want running", got)
	}
}

// Pids are recycled. A command that started moments ago is not judged, because
// the pid it recorded may since have been handed to something else — and the
// wrong answer here lands on the command the user is watching right now.
func TestACommandThatJustStartedIsNotJudged(t *testing.T) {
	d := testDaemon(t)
	startCommand(t, d, "fresh", d.deviceID, deadPID(t), event.NowMillis())

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("swept a command that started just now")
	}
	if got := statusOf(t, d, "fresh"); got != store.StatusRunning {
		t.Errorf("status is %q, want running", got)
	}
}

// The one that matters for a cross-machine tool: pids are only meaningful on the
// machine that issued them. A command running on another device must never be
// judged against this device's process table, or every sync would orphan the
// peer's running commands — and, worse, a coincidental local pid would silently
// keep a genuinely dead one alive.
func TestAPeersRunningCommandIsNeverJudgedHere(t *testing.T) {
	d := testDaemon(t)
	// A pid that is dead *here*. On the peer it is the shell that is still
	// running the command.
	startCommand(t, d, "peer1", "some-other-device", deadPID(t), longAgo())
	// And one of ours, so the sweep has something to do and cannot pass by
	// simply doing nothing at all.
	startCommand(t, d, "mine", d.deviceID, deadPID(t), longAgo())

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d command(s), want only our own", n)
	}
	if got := statusOf(t, d, "peer1"); got != store.StatusRunning {
		t.Errorf("a peer's command was marked %q from this machine's pid table", got)
	}
	if got := statusOf(t, d, "mine"); got != store.StatusOrphaned {
		t.Errorf("our own command is %q, want orphaned", got)
	}
}

// An imported command, or one from a hook that could not report a pid, carries
// no shell to ask about, and a malformed event could carry anything at all.
//
// A negative value is the one that bites: kill(-n, 0) asks about process *group*
// n, so a command claiming pgid -424242 would be judged against a group that has
// nothing to do with it — orphaned if that group happens not to exist, held
// alive if it does. 0 and 1 are here for completeness; both answer "alive" on
// their own, so the guard is what makes the negative case safe.
func TestACommandWithNoKnownShellIsLeftRunning(t *testing.T) {
	d := testDaemon(t)
	for _, pgid := range []int{0, 1, -1, -424242} {
		id := fmt.Sprintf("nopid%d", pgid)
		startCommand(t, d, id, d.deviceID, pgid, longAgo())
	}

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("swept %d command(s) with no usable pid, want 0", n)
	}
}

// The sweep runs every minute forever. The second pass over the same command
// must find nothing to do, or the event log would grow an orphan event a minute
// for the rest of the database's life.
func TestSweepingTwiceOrphansOnce(t *testing.T) {
	d := testDaemon(t)
	startCommand(t, d, "c1", d.deviceID, deadPID(t), longAgo())

	if _, err := d.SweepOrphans(); err != nil {
		t.Fatal(err)
	}
	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the second sweep marked %d command(s) again", n)
	}

	var orphans int
	if err := d.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE type = ?`, event.TypeOrphan).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 1 {
		t.Errorf("the log holds %d orphan events for one command", orphans)
	}
}

// A command that finished is settled, whatever became of the shell afterwards.
// Closing a terminal must not rewrite the history of what ran in it.
func TestAFinishedCommandIsNotTouched(t *testing.T) {
	d := testDaemon(t)
	at := longAgo()
	startCommand(t, d, "done", d.deviceID, deadPID(t), at)
	end, err := json.Marshal(event.EndPayload{EndTime: at + 100, ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.AppendEvent(event.Event{
		EventID: "done-end", CommandID: "done", DeviceID: d.deviceID,
		Type: event.TypeEnd, Payload: end, CreatedAt: at + 100,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := d.SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("swept %d finished command(s)", n)
	}
	if got := statusOf(t, d, "done"); got != store.StatusCompleted {
		t.Errorf("a finished command became %q", got)
	}
}

// A process we are not allowed to signal still exists, and existing is the whole
// question. Reading EPERM as "gone" would orphan commands run under any shell
// belonging to another user — su, a login shell, a shared box.
func TestAShellWeCannotSignalCountsAsAlive(t *testing.T) {
	if syscall.Kill(1, 0) == nil {
		t.Skip("running as root: pid 1 is signallable, so there is no EPERM to test")
	}
	if !shellAlive(1) {
		t.Error("a process we may not signal was read as gone")
	}
}
