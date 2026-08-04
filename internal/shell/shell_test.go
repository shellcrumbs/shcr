package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// /proc/<pid>/cmdline is world-readable on Linux while /proc/<pid>/environ is
// not, so command text must never be passed to `shcr event` as an argument.
// Otherwise every recorded command — including shell builtins like
// `export TOKEN=...`, which never appear in the process list on their own — is
// published to every other account on the machine for a few milliseconds.
func TestHooksNeverPutCommandTextInArgv(t *testing.T) {
	for _, sh := range Supported {
		script, err := InitScript(sh)
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		if strings.Contains(script, "--command") {
			t.Errorf("%s hook passes the command as an argument; use the environment instead", sh)
		}
		if !strings.Contains(script, "SHCR_COMMAND") {
			t.Errorf("%s hook does not pass the command through the environment", sh)
		}
	}
}

// Both hooks have a fallback clock for shells without EPOCHREALTIME — bash
// before 5.0, which is what macOS ships, and a zsh where zsh/datetime failed to
// load. SECONDS is not a substitute for a wall clock: it counts from when the
// shell started, so every command would be recorded as having run a few seconds
// after 1970.
func TestFallbackClockIsWallTimeNotShellUptime(t *testing.T) {
	for _, tc := range []struct{ shell, bin, prelude string }{
		// Unsetting EPOCHREALTIME leaves it empty for the rest of the shell,
		// which is exactly how bash 4 behaves.
		{"bash", "bash", "unset EPOCHREALTIME"},
		// zsh's EPOCHREALTIME is read-only, so the way to reproduce a failed
		// module load is to leave the module somewhere zsh cannot find it.
		{"zsh", "zsh", "module_path=(/nonexistent)"},
	} {
		bin, err := exec.LookPath(tc.bin)
		if err != nil {
			t.Logf("%s is not installed; skipping", tc.bin)
			continue
		}
		script, err := InitScript(tc.shell)
		if err != nil {
			t.Fatalf("%s: %v", tc.shell, err)
		}
		path := filepath.Join(t.TempDir(), tc.shell)
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}

		// __SHCR_BIN is redirected after loading so the hooks this installs
		// cannot spawn the test binary at us.
		out, err := exec.Command(bin, "-c", tc.prelude+"\n"+
			"source "+path+" 2>/dev/null\n"+
			"__SHCR_BIN=/bin/true\n"+
			"__shcr_now_ms\n"+
			"echo $__shcr_ms\n").Output()
		if err != nil {
			t.Errorf("%s: %v", tc.shell, err)
			continue
		}
		ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			t.Errorf("%s printed %q, not a timestamp", tc.shell, out)
			continue
		}
		if now := time.Now().UnixMilli(); ms < now-60_000 || ms > now+60_000 {
			t.Errorf("%s fallback clock returned %d, which is %s — want a time near now (%d)",
				tc.shell, ms, time.UnixMilli(ms).UTC().Format("2006-01-02"), now)
		}
	}
}

// fish is not exercised against a real shell anywhere, so these two are checked
// by reading the script. Both are silent failures: `exit` in a sourced file
// closes the user's shell rather than returning from the file, and a bare
// command substitution splits the prompt buffer on newlines — expanding to no
// argument at all when the buffer is empty, which leaves --query with nothing
// to consume and the picker refusing to start.
func TestFishScriptAvoidsTheTwoSourcedFileTraps(t *testing.T) {
	script, err := InitScript("fish")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "exit ") {
			t.Errorf("a sourced file must return, not exit: %q", strings.TrimSpace(line))
		}
	}
	if strings.Contains(script, "--query (commandline)") {
		t.Error("the prompt buffer reaches --query unquoted; collect it into one argument")
	}
	if !strings.Contains(script, "string collect") {
		t.Error("the prompt buffer should be passed through `string collect`")
	}
}

// The templates wrap the binary path in single quotes and the path went in
// unescaped, so a path containing a quote closed the string and the rest of it
// became code — executed by the eval in the user's shell rc, on every shell.
func TestBinaryPathCannotBreakOutOfTheHookQuoting(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	for _, path := range []string{
		`/home/o'brien/bin/shcr`,
		`/tmp/x';echo INJECTED;'y/shcr`,
		`/tmp/a"b/shcr`,
		`/tmp/$(echo no)/shcr`,
		"/tmp/back`tick`/shcr",
		`/usr/local/bin/shcr`,
	} {
		// What the templates do, with the path escaped the way InitScript does.
		script := "__SHCR_BIN='" + shellQuote(path) + "'\nprintf %s \"$__SHCR_BIN\""
		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		if err != nil {
			t.Errorf("path %q produced a script bash could not run: %v (%s)", path, err, out)
			continue
		}
		if string(out) != path {
			t.Errorf("path %q came back as %q", path, out)
		}
	}
}

func TestInitScriptsAreSelfGuardedAndNameTheBinary(t *testing.T) {
	for _, sh := range Supported {
		script, err := InitScript(sh)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, "@SHCR_BIN@") {
			t.Errorf("%s hook still has the unsubstituted binary placeholder", sh)
		}
		if !strings.Contains(script, "__SHCR_LOADED") {
			t.Errorf("%s hook has no double-load guard", sh)
		}
	}
	if _, err := InitScript("csh"); err == nil {
		t.Error("an unsupported shell should be an error")
	}
}
