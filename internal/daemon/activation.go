package daemon

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// The first file descriptor a service manager hands over. Defined by the
// systemd socket-activation protocol; 0, 1 and 2 remain stdio.
const listenFDStart = 3

// listenerFromServiceManager returns the socket systemd pre-opened for us, or
// nil when the daemon was started by hand.
//
// Socket activation is worth the twenty lines: the socket then exists from boot,
// so the first command of a login never falls back to the spool, and systemd
// owns creating and removing it — which is exactly the stale-socket problem we
// otherwise have to guess our way through after a crash.
func listenerFromServiceManager() (net.Listener, error) {
	pid := os.Getenv("LISTEN_PID")
	fds := os.Getenv("LISTEN_FDS")
	if pid == "" || fds == "" {
		return nil, nil
	}
	// The variables are inherited by children, so a mismatched pid means they
	// were meant for an ancestor and not for us.
	if pid != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	n, err := strconv.Atoi(fds)
	if err != nil || n < 1 {
		return nil, nil
	}
	// Clear them so nothing downstream mistakes them for its own.
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")

	f := os.NewFile(listenFDStart, "shcr.sock")
	ln, err := net.FileListener(f)
	// FileListener dups the descriptor, so the original is ours to close.
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("adopting the socket from the service manager: %w", err)
	}
	return ln, nil
}
