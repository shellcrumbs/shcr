// Package service installs shcr as a systemd user service.
//
// A user unit rather than a system one: everything the daemon touches belongs
// to one person — the database under their home, the socket in their runtime
// directory, their login keyring, and an orphan sweep that signals their
// shells. A system unit would have to reconstruct all of that per user and
// would gain nothing.
package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/shellcrumbs/shcr/internal/paths"
)

const (
	SocketUnit  = "shcr.socket"
	ServiceUnit = "shcr.service"
)

// Socket activation means the socket exists from boot, so the first command of
// a session never falls back to the spool, and systemd owns creating and
// removing it. %t is the user's runtime directory, which is exactly where the
// hooks look for the socket.
const socketTemplate = `[Unit]
Description=Shellcrumbs capture socket
Documentation=https://github.com/shellcrumbs/shcr

[Socket]
ListenStream=%t/shcr.sock
SocketMode=0600
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
`

// The daemon is started by the socket but does not stop with it: the orphan
// sweep has to run every minute and sync on its own schedule, so this is
// activation-to-start rather than activation-on-demand.
//
// Deliberately absent: PrivateUsers, which would make the orphan sweep's
// kill(pid, 0) fail with EPERM against the user's own shells — and EPERM reads
// as "still alive", so every orphan would look like it was still running,
// silently and forever. ProtectHome is absent for the obvious reason.
const serviceTemplate = `[Unit]
Description=Shellcrumbs capture daemon
Documentation=https://github.com/shellcrumbs/shcr
Requires={{.SocketUnit}}
After={{.SocketUnit}}
# These belong in [Unit], not [Service]. Put them in the wrong section and
# systemd ignores them with a warning nobody reads, leaving a database this
# process cannot open to restart forever.
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
ExecStart={{.Binary}} daemon
Restart=on-failure
RestartSec=2

NoNewPrivileges=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
LockPersonality=yes
{{- if .AddressFamilies}}
RestrictAddressFamilies={{.AddressFamilies}}
{{- end}}
# Far above what the daemon uses at rest, low enough to catch a runaway leak.
MemoryMax=192M

[Install]
WantedBy=default.target
`

type params struct {
	Binary          string
	SocketUnit      string
	AddressFamilies string
}

// UnitDir is where systemd looks for a user's own units.
func UnitDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

// Render produces both unit files.
func Render(binary string, networkBackend bool) (socket, service string, err error) {
	// AF_UNIX covers the hooks and the file sync backend. A cloud backend needs
	// the internet families, and asking for them only when they are used keeps
	// the default install unable to open a network socket at all.
	families := "AF_UNIX"
	if networkBackend {
		families += " AF_INET AF_INET6"
	}
	p := params{Binary: binary, SocketUnit: SocketUnit, AddressFamilies: families}

	var sb, svb bytes.Buffer
	st := template.Must(template.New("socket").Parse(socketTemplate))
	if err := st.Execute(&sb, p); err != nil {
		return "", "", err
	}
	svt := template.Must(template.New("service").Parse(serviceTemplate))
	if err := svt.Execute(&svb, p); err != nil {
		return "", "", err
	}
	return sb.String(), svb.String(), nil
}

// Install writes both units and reports where they went.
func Install(binary string, networkBackend bool) ([]string, error) {
	socket, service, err := Render(binary, networkBackend)
	if err != nil {
		return nil, err
	}
	dir := UnitDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A slice, not a map: ranging over a map randomises the order, so the list
	// of files this reports having written came out differently every run.
	var written []string
	for _, u := range []struct{ name, body string }{
		{SocketUnit, socket},
		{ServiceUnit, service},
	} {
		name, body := u.name, u.body
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return written, err
		}
		written = append(written, p)
	}
	return written, nil
}

// Uninstall stops the units and removes them. Data is never touched: removing a
// service should not delete a history the user may still want.
func Uninstall() ([]string, error) {
	_ = Systemctl("disable", "--now", ServiceUnit)
	_ = Systemctl("disable", "--now", SocketUnit)

	var removed []string
	for _, name := range []string{ServiceUnit, SocketUnit} {
		p := filepath.Join(UnitDir(), name)
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if !os.IsNotExist(err) {
			return removed, err
		}
	}
	_ = Systemctl("daemon-reload")
	return removed, nil
}

// Systemctl runs a user-scope systemctl command.
func Systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Available reports whether a systemd user manager is running for this session.
func Available() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl is not on PATH; this machine does not appear to use systemd")
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return fmt.Errorf("XDG_RUNTIME_DIR is unset, so there is no user session to install into")
	}
	cmd := exec.Command("systemctl", "--user", "is-system-running")
	out, _ := cmd.CombinedOutput()
	// Any answer means a user manager replied, and every state it can report is
	// one we can install into — "degraded" just means some other unit failed.
	// Silence is the only answer that says there is nothing there.
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("no systemd user manager is reachable for this session")
	}
	return nil
}

// LingerHint explains whether the daemon will survive logout, which is the
// difference between a laptop and a machine you only ever ssh into.
func LingerHint(user string) string {
	out, err := exec.Command("loginctl", "show-user", user, "-p", "Linger", "--value").Output()
	if err != nil {
		return ""
	}
	if strings.TrimSpace(string(out)) == "yes" {
		return ""
	}
	return "The daemon stops when your last session ends, so a machine you only ssh into\n" +
		"will not sweep orphans or sync while you are away. To keep it running:\n" +
		"    sudo loginctl enable-linger " + user
}

// BinaryPath is the absolute path of the running binary, which is what the unit
// must point at.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// A unit pointing into a build tree works but is a trap; say so at install
	// time rather than when the next `go build` truncates the file.
	return exe, nil
}

// InTree reports whether the binary looks like it lives in a build directory
// rather than somewhere installed.
func InTree(binary string) bool {
	home, _ := os.UserHomeDir()
	stable := []string{"/usr/bin", "/usr/local/bin", filepath.Join(home, ".local", "bin"), "/opt"}
	dir := filepath.Dir(binary)
	for _, s := range stable {
		if dir == s {
			return false
		}
	}
	return true
}

// DataPaths are the directories the user may want to remove after uninstalling.
func DataPaths() []string { return []string{paths.DataDir(), paths.StateDir()} }
