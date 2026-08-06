package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func atTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SHCR_DATA_DIR", dir)
	t.Setenv("XDG_DATA_HOME", dir)
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The hostname is shared unless asked otherwise. A device id is a UUID, and a
// list of UUIDs cannot tell you which machine is which — which is the question
// `shcr sync status` and the dashboard's peer list exist to answer.
func TestTheHostnameIsSharedByDefault(t *testing.T) {
	atTempDir(t)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sync.HideHostname {
		t.Error("no config at all should still share the hostname")
	}

	// A config that says nothing about it means the default, not off.
	writeConfig(t, `{"sync":{"enabled":true,"backend":"file","path":"/tmp/b"}}`)
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sync.HideHostname {
		t.Error("a config that does not mention it should share the hostname")
	}
}

func TestHidingTheHostnameIsHonoured(t *testing.T) {
	atTempDir(t)
	writeConfig(t, `{"sync":{"enabled":true,"backend":"file","path":"/tmp/b","hide_hostname":true}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Sync.HideHostname {
		t.Error("hide_hostname:true was ignored")
	}
}

// The setting used to be `share_hostname`, off by default. Anyone carrying a
// config from before the change has that key sitting in it, and it no longer
// means anything — the hostname starts being shared. Asserted rather than left
// implicit, because it is a privacy-relevant default changing under an existing
// install.
func TestAnOldShareHostnameKeyNoLongerApplies(t *testing.T) {
	atTempDir(t)
	writeConfig(t, `{"sync":{"enabled":true,"backend":"file","path":"/tmp/b","share_hostname":false}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sync.HideHostname {
		t.Fatal("the old key was somehow honoured; this test is out of date")
	}
	// The rest of the config still loads, which is the part that must not break.
	if c.Sync.Backend != "file" || c.Sync.Path != "/tmp/b" || !c.Sync.Enabled {
		t.Errorf("an old config stopped loading: %+v", c.Sync)
	}
}

// Sharing is the default, so it should not be written out — a config file
// should record decisions, not defaults.
func TestSharingIsNotWrittenToTheFile(t *testing.T) {
	atTempDir(t)
	if err := Save(Config{Sync: Sync{Enabled: true, Backend: "gcs", Bucket: "b"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hide_hostname") {
		t.Errorf("the default was written out:\n%s", b)
	}

	if err := Save(Config{Sync: Sync{Enabled: true, Backend: "gcs", Bucket: "b", HideHostname: true}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(Path())
	if !strings.Contains(string(b), `"hide_hostname": true`) {
		t.Errorf("a decision was not written out:\n%s", b)
	}
}

func TestSyncSettingsRoundTrip(t *testing.T) {
	atTempDir(t)
	want := Config{
		Sync:    Sync{Enabled: true, Backend: "gcs", Bucket: "shellcrumbs", Prefix: "shcr", HideHostname: true},
		Ranking: Ranking{LogAcceptances: false},
	}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The ranking default is applied after unmarshalling, which is the only way a
// field the file sets to false can be told apart from one it omits.
func TestRankingDefaultsOnlyFillGaps(t *testing.T) {
	atTempDir(t)
	writeConfig(t, `{"sync":{"backend":"file"}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Ranking.LogAcceptances {
		t.Error("a config that does not mention ranking should get the default, which is on")
	}

	writeConfig(t, `{"ranking":{"log_acceptances":false}}`)
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Ranking.LogAcceptances {
		t.Error("an explicit false was overwritten by the default")
	}
}

func TestABrokenConfigSaysWhichFile(t *testing.T) {
	atTempDir(t)
	writeConfig(t, `{"sync":`)
	_, err := Load()
	if err == nil {
		t.Fatal("no error on malformed json")
	}
	if !strings.Contains(err.Error(), "config.json") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

// Nothing in here is secret, but it sits beside the key file and records where
// history goes.
func TestTheConfigIsNotWorldReadable(t *testing.T) {
	atTempDir(t)
	if err := Save(Config{Sync: Sync{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode %04o lets other accounts read it", perm)
	}
}

// Guards the assumption the tests above rest on: these really are the fields
// that get persisted, so a new one added without a test is visible here.
func TestConfigShape(t *testing.T) {
	b, err := json.Marshal(Config{Sync: Sync{HideHostname: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enabled", "backend", "path", "hide_hostname", "log_acceptances"} {
		if !strings.Contains(string(b), key) {
			t.Errorf("%q is missing from %s", key, b)
		}
	}
}
