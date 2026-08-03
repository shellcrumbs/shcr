package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func render(t *testing.T, network bool) (socket, service string) {
	t.Helper()
	s, sv, err := Render("/home/u/.local/bin/shcr", network)
	if err != nil {
		t.Fatal(err)
	}
	return s, sv
}

// section returns the body of one ini section.
//
// Line-based on purpose: a substring search finds "[Service]" inside a comment
// that merely mentions it, and then reports the whole file as that section.
func section(unit, name string) string {
	var out []string
	in := false
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			in = trimmed == "["+name+"]"
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// StartLimit* live in [Unit]. systemd accepts them in [Service] only by
// ignoring them, with a warning in the journal that nobody reads — leaving a
// database this process cannot open to restart forever.
func TestStartLimitIsInTheUnitSection(t *testing.T) {
	_, service := render(t, false)
	for _, key := range []string{"StartLimitIntervalSec", "StartLimitBurst"} {
		if !strings.Contains(section(service, "Unit"), key) {
			t.Errorf("%s is not in [Unit], so systemd will ignore it", key)
		}
		if strings.Contains(section(service, "Service"), key) {
			t.Errorf("%s is in [Service], where systemd ignores it", key)
		}
	}
	if !strings.Contains(section(service, "Service"), "Restart=on-failure") {
		t.Error("no restart policy")
	}
}

// Two sandboxing options look attractive and would quietly break the tool.
func TestServiceAvoidsSandboxingThatBreaksIt(t *testing.T) {
	_, service := render(t, false)
	// PrivateUsers makes the orphan sweep's kill(pid,0) return EPERM against the
	// user's own shells, and EPERM is read as "still alive" — so orphan
	// detection would stop working with no error anywhere.
	if strings.Contains(service, "PrivateUsers") {
		t.Error("PrivateUsers would silently disable orphan detection")
	}
	// The database lives under $HOME.
	if strings.Contains(service, "ProtectHome") {
		t.Error("ProtectHome would hide the database from the daemon")
	}
	// ProtectSystem=strict would break a file sync backend pointed outside $HOME
	// unless the install also emits a matching ReadWritePaths.
	if strings.Contains(service, "ProtectSystem=strict") && !strings.Contains(service, "ReadWritePaths") {
		t.Error("ProtectSystem=strict without ReadWritePaths would break the file sync backend")
	}
}

func TestAddressFamiliesFollowTheBackend(t *testing.T) {
	_, local := render(t, false)
	if !strings.Contains(local, "RestrictAddressFamilies=AF_UNIX\n") {
		t.Error("a local-only install should not be allowed to open network sockets")
	}
	_, network := render(t, true)
	if !strings.Contains(network, "AF_INET") {
		t.Error("a network backend needs the internet families")
	}
}

// The socket unit must name the same path the hooks connect to.
func TestSocketUnitMatchesTheHookPath(t *testing.T) {
	socket, _ := render(t, false)
	// %t expands to the user's runtime directory, which is where paths.SocketPath
	// looks when XDG_RUNTIME_DIR is set.
	if !strings.Contains(socket, "ListenStream=%t/shcr.sock") {
		t.Error("socket unit does not listen where the hooks connect")
	}
	if !strings.Contains(socket, "SocketMode=0600") {
		t.Error("the socket must not be readable by other users")
	}
	if !strings.Contains(socket, "RemoveOnStop=yes") {
		t.Error("systemd should clean the socket up, since the daemon no longer does")
	}
	if !strings.Contains(socket, "WantedBy=sockets.target") {
		t.Error("the socket must come up at login for the first command to reach the daemon")
	}
}

// The daemon must be started by the socket but not stop with it: the orphan
// sweep runs every minute and sync on its own schedule.
func TestServiceIsBoundToTheSocket(t *testing.T) {
	_, service := render(t, false)
	unit := section(service, "Unit")
	if !strings.Contains(unit, "Requires="+SocketUnit) || !strings.Contains(unit, "After="+SocketUnit) {
		t.Error("service is not ordered after its socket")
	}
	if strings.Contains(service, "StopWhenUnneeded") || strings.Contains(service, "RuntimeMaxSec") {
		t.Error("the daemon must keep running; sweeps and sync are on timers")
	}
}

// The strongest check available: hand the units to systemd itself.
func TestSystemdAcceptsTheUnits(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not available")
	}
	dir := t.TempDir()
	socket, service := render(t, false)
	socketPath := filepath.Join(dir, SocketUnit)
	servicePath := filepath.Join(dir, ServiceUnit)
	if err := os.WriteFile(socketPath, []byte(socket), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, []byte(service), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("systemd-analyze", "--user", "verify", servicePath, socketPath).CombinedOutput()
	text := string(out)
	// A missing ExecStart binary is expected here; anything else is ours.
	var complaints []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" || strings.Contains(line, "Command ") || strings.Contains(line, "not executable") {
			continue
		}
		complaints = append(complaints, line)
	}
	if len(complaints) > 0 {
		t.Errorf("systemd objected to the units:\n  %s", strings.Join(complaints, "\n  "))
	}
	_ = err // verify exits non-zero for the missing binary alone
}

// Install used to range over a map, so the files it reported writing came back
// in a different order most runs — output that changes for no reason reads like
// something changed.
func TestInstallReportsFilesInAStableOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var first []string
	for i := 0; i < 8; i++ {
		written, err := Install("/usr/local/bin/shcr", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(written) != 2 {
			t.Fatalf("wrote %d files, want 2: %v", len(written), written)
		}
		if i == 0 {
			first = written
			if filepath.Base(written[0]) != SocketUnit || filepath.Base(written[1]) != ServiceUnit {
				t.Errorf("want the socket before the service, got %v", written)
			}
			continue
		}
		for j := range written {
			if written[j] != first[j] {
				t.Fatalf("run %d reported a different order:\n  %v\n  %v", i, first, written)
			}
		}
	}
}

func TestInTreeSpotsABuildDirectory(t *testing.T) {
	home, _ := os.UserHomeDir()
	if InTree(filepath.Join(home, ".local", "bin", "shcr")) {
		t.Error("~/.local/bin is an installed location")
	}
	if InTree("/usr/local/bin/shcr") {
		t.Error("/usr/local/bin is an installed location")
	}
	if !InTree(filepath.Join(home, "shcr", "bin", "shcr")) {
		t.Error("a build tree should be flagged: rebuilding replaces the running binary")
	}
}
