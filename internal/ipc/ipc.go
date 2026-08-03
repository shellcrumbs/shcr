// Package ipc is the hook-side half of the local transport: it hands an event
// to the daemon over the unix socket, and falls back to a spool file when the
// daemon is not there. The shell must never block and must never lose an event.
package ipc

import (
	"encoding/json"
	"net"
	"os"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/paths"
)

const dialTimeout = 200 * time.Millisecond

// Nudge tells the daemon that something worth syncing on just happened.
//
// Unlike an event it is not spooled when the daemon is down: a trigger is a
// statement about right now, and replaying "a terminal opened" an hour later
// would be noise. The daemon triggers a sync on its own start anyway, which
// covers exactly the case where nobody was listening.
func Nudge(reason string) error {
	conn, err := net.DialTimeout("unix", paths.SocketPath(), dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(dialTimeout))
	line, err := json.Marshal(map[string]string{"nudge": reason})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(line, '\n'))
	return err
}

// Send delivers an event, spooling it if the daemon is unreachable. It reports
// whether the daemon took it directly.
func Send(ev event.Event) (delivered bool, err error) {
	line, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	line = append(line, '\n')

	conn, derr := net.DialTimeout("unix", paths.SocketPath(), dialTimeout)
	if derr == nil {
		defer conn.Close()
		_ = conn.SetWriteDeadline(time.Now().Add(dialTimeout))
		if _, werr := conn.Write(line); werr == nil {
			return true, nil
		}
	}
	return false, spool(line)
}

// spool appends to the replay file. A single O_APPEND write of one line keeps
// concurrent hooks from interleaving.
func spool(line []byte) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	f, err := os.OpenFile(paths.SpoolPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}
