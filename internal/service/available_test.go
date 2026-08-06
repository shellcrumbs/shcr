package service

import (
	"strings"
	"testing"
)

// A machine with systemctl on PATH but no reachable user manager must be
// refused. systemctl answers such a request on stderr, so reading stdout and
// stderr together counted the error message itself as an answer.
func TestAnUnreachableUserManagerIsNotAvailable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	if err := Available(); err == nil {
		t.Error("reported systemd as available with no user manager reachable")
	}
}

// systemd splits ExecStart on whitespace, so an unquoted path with a space in
// it runs the wrong binary with the rest as arguments.
func TestAPathWithSpacesStaysOneArgument(t *testing.T) {
	_, svc, err := Render("/home/u/my apps/shcr", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svc, `ExecStart="/home/u/my apps/shcr" daemon`) {
		t.Errorf("the path is not quoted:\n%s", execStartLine(svc))
	}
}

// Quoting an ordinary path costs nothing and saves deciding which kind it is.
func TestAnOrdinaryPathIsQuotedToo(t *testing.T) {
	_, svc, err := Render("/home/u/.local/bin/shcr", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svc, `ExecStart="/home/u/.local/bin/shcr" daemon`) {
		t.Errorf("got %q", execStartLine(svc))
	}
}

// A quote or a backslash would end or escape its way out of the quoting.
func TestRenderRefusesAPathItCannotQuote(t *testing.T) {
	for _, bad := range []string{
		"/home/u/sh\"cr",
		`/home/u/sh\cr`,
		"/home/u/shcr\nExecStartPre=/bin/evil",
	} {
		if _, _, err := Render(bad, false); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func execStartLine(unit string) string {
	for _, l := range strings.Split(unit, "\n") {
		if strings.HasPrefix(l, "ExecStart") {
			return l
		}
	}
	return "(no ExecStart)"
}
