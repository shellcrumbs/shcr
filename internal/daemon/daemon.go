// Package daemon runs the long-lived local process: it accepts events from the
// shell hooks over a unix socket, drains anything the hooks spooled while it was
// down, and periodically sweeps for commands whose shell died mid-run.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/paths"
	"github.com/shellcrumbs/shcr/internal/redact"
	"github.com/shellcrumbs/shcr/internal/store"
)

const (
	sweepInterval = 60 * time.Second
	// A command is not considered for orphaning until it has been running this
	// long, which keeps a pid recycled onto a dead shell from causing a false
	// positive on a command that just started.
	sweepGrace = 5 * time.Second
)

type Daemon struct {
	store    *store.Store
	deviceID string
	sockPath string
	logger   *log.Logger
	redactor *redact.Redactor

	// OnTrigger, when set, is told about moments worth syncing on: a command
	// recorded, a shell opening or closing, the picker being opened.
	OnTrigger func(reason string)
}

func New(st *store.Store, deviceID string, logger *log.Logger, r *redact.Redactor) *Daemon {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if r == nil {
		r = redact.New(nil)
	}
	return &Daemon{
		store: st, deviceID: deviceID, sockPath: paths.SocketPath(),
		logger: logger, redactor: r,
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	ln, err := listenerFromServiceManager()
	if err != nil {
		return err
	}
	activated := ln != nil
	if !activated {
		if err := os.MkdirAll(filepath.Dir(d.sockPath), 0o700); err != nil {
			return err
		}
		if err := d.clearStaleSocket(); err != nil {
			return err
		}
		if ln, err = net.Listen("unix", d.sockPath); err != nil {
			return fmt.Errorf("listen %s: %w", d.sockPath, err)
		}
		if err := os.Chmod(d.sockPath, 0o600); err != nil {
			return err
		}
	}
	defer ln.Close()

	if activated {
		d.logger.Printf("adopted socket %s from the service manager (device %s)", ln.Addr(), d.deviceID)
	} else {
		d.logger.Printf("listening on %s (device %s)", d.sockPath, d.deviceID)
	}

	if n, err := d.DrainSpool(); err != nil {
		d.logger.Printf("spool drain: %v", err)
	} else if n > 0 {
		d.logger.Printf("replayed %d spooled event(s)", n)
	}
	if n, err := d.SweepOrphans(); err != nil {
		d.logger.Printf("startup sweep: %v", err)
	} else if n > 0 {
		d.logger.Printf("marked %d command(s) orphaned at startup", n)
	}

	go d.sweepLoop(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				d.logger.Printf("shutting down")
				return nil
			}
			return err
		}
		go d.handleConn(conn)
	}
}

// clearStaleSocket removes a socket left behind by a crashed daemon, but never
// one that another live daemon is still serving.
func (d *Daemon) clearStaleSocket() error {
	if _, err := os.Stat(d.sockPath); err != nil {
		return nil
	}
	c, err := net.DialTimeout("unix", d.sockPath, 250*time.Millisecond)
	if err == nil {
		c.Close()
		return fmt.Errorf("another shcr daemon is already listening on %s", d.sockPath)
	}
	return os.Remove(d.sockPath)
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // commands can be big heredocs
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := d.ingest(line); err != nil {
			d.logger.Printf("ingest: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
}

// control is the non-event traffic on the socket. A bare event carries no
// "nudge" field, so the two shapes are told apart without a protocol version.
type control struct {
	Nudge string `json:"nudge"`
}

func (d *Daemon) ingest(line []byte) error {
	var ctl control
	if err := json.Unmarshal(line, &ctl); err == nil && ctl.Nudge != "" {
		if d.OnTrigger != nil {
			d.OnTrigger(ctl.Nudge)
		}
		return nil
	}

	var ev event.Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return fmt.Errorf("bad event json: %w", err)
	}
	if ev.EventID == "" || ev.CommandID == "" || ev.Type == "" {
		return errors.New("event missing required fields")
	}
	scrubbed, err := d.scrub(ev)
	if err != nil {
		return err
	}
	if scrubbed == nil {
		return nil // a skip rule matched; record nothing
	}
	if _, err := d.store.AppendEvent(*scrubbed); err != nil {
		return err
	}
	if d.OnTrigger != nil {
		d.OnTrigger("command")
	}
	return nil
}

// scrub re-runs redaction at the last point before SQLite. The sender already
// did this, which is what keeps secrets out of the spool file, but the daemon is
// the boundary the guarantee is stated at — and an older shcr binary, or a hook
// someone wrote themselves, could still deliver raw text here. Redaction is
// idempotent, so the second pass costs nothing on already-clean input.
func (d *Daemon) scrub(ev event.Event) (*event.Event, error) {
	if ev.Type != event.TypeStart {
		return &ev, nil
	}
	var p event.StartPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, fmt.Errorf("start payload: %w", err)
	}
	text, action, fired := d.redactor.Apply(p.Command)
	switch action {
	case redact.ActionNone:
		return &ev, nil
	case redact.ActionSkip:
		d.logger.Printf("skipped a command (%s)", strings.Join(fired, ","))
		return nil, nil
	}
	p.Command = text
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	ev.Payload = payload
	d.logger.Printf("redacted a command (%s)", strings.Join(fired, ","))
	return &ev, nil
}

// DrainSpool replays events the hooks wrote while the daemon was unavailable.
// Replay is safe at any time because AppendEvent ignores known event ids.
func (d *Daemon) DrainSpool() (int, error) {
	p := paths.SpoolPath()
	tmp := p + ".draining"

	// A .draining file left behind means a previous daemon died partway through
	// replaying it. Its events were never ingested and the hooks have long since
	// moved on to a fresh spool, so without this they would be lost — the one
	// case the spool exists to cover.
	n, err := d.drainFile(tmp)
	if err != nil {
		return n, err
	}

	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return n, nil
		}
		return n, err
	}
	// Move it aside first so hooks writing concurrently start a fresh file and
	// nothing is lost to a truncate race.
	if err := os.Rename(p, tmp); err != nil {
		f.Close()
		return n, err
	}
	f.Close()

	more, err := d.drainFile(tmp)
	return n + more, err
}

// drainFile ingests one spool file and removes it, reporting how many events it
// replayed. A file that is not there is not an error: it is the normal case.
func (d *Daemon) drainFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() {
		f.Close()
		os.Remove(path)
	}()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if err := d.ingest(sc.Bytes()); err != nil {
			d.logger.Printf("spool line: %v", err)
			continue
		}
		n++
	}
	return n, sc.Err()
}

func (d *Daemon) sweepLoop(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := d.SweepOrphans(); err != nil {
				d.logger.Printf("sweep: %v", err)
			} else if n > 0 {
				d.logger.Printf("marked %d command(s) orphaned", n)
			}
		}
	}
}

// SweepOrphans flips still-running commands to orphaned when the shell process
// group that started them no longer exists.
func (d *Daemon) SweepOrphans() (int, error) {
	live, err := d.store.LiveCommands(d.deviceID)
	if err != nil {
		return 0, err
	}
	cutoff := event.NowMillis() - sweepGrace.Milliseconds()
	n := 0
	for _, c := range live {
		if c.PGID <= 1 || c.StartTime > cutoff {
			continue
		}
		if shellAlive(c.PGID) {
			continue
		}
		ev, err := event.New(c.ID, d.deviceID, event.TypeOrphan, map[string]any{
			"detected_at": event.NowMillis(),
			"reason":      "shell gone",
		})
		if err != nil {
			return n, err
		}
		if _, err := d.store.AppendEvent(ev); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// shellAlive reports whether the shell that started a command still exists.
//
// The question that decides `orphaned` is not "is the work still running" but
// "can an end event ever arrive" — only the shell sends one, so once it is gone
// the command's outcome is unknowable no matter what its children are doing.
// Testing the whole process group instead gets this wrong in the exact case
// that matters: kill a terminal during `sleep 100` and the reparented sleep
// keeps the group alive, so the command would sit at `running` forever.
//
// A hook can only ever report the shell's own pid; it cannot know the process
// group the job will land in. For an interactive shell the two coincide, since
// the shell leads its own group.
//
// EPERM counts as alive: the process exists, we simply may not signal it.
func shellAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
