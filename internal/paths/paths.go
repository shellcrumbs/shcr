// Package paths resolves the handful of on-disk locations shcr uses and owns
// the per-install device identity.
package paths

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shellcrumbs/shcr/internal/event"
)

// DataDir holds durable state: the database and the device id.
func DataDir() string {
	if d := os.Getenv("SHCR_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "shcr")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "shcr")
}

// StateDir holds things that may be discarded between boots: the spool file and
// the socket when there is no runtime dir.
func StateDir() string {
	if d := os.Getenv("SHCR_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "shcr")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "shcr")
}

func DBPath() string { return filepath.Join(DataDir(), "shcr.db") }

func SocketPath() string {
	if p := os.Getenv("SHCR_SOCKET"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "shcr.sock")
	}
	return filepath.Join(StateDir(), "shcr.sock")
}

func SpoolPath() string { return filepath.Join(StateDir(), "spool.jsonl") }

func EnsureDirs() error {
	for _, d := range []string{DataDir(), StateDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// DeviceID returns this install's stable identity, creating it on first call.
// It deliberately outlives hostname changes.
func DeviceID() (string, error) {
	if err := EnsureDirs(); err != nil {
		return "", err
	}
	p := filepath.Join(DataDir(), "device_id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	id := event.NewID()
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}
