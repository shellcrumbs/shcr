package service

import "testing"

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
