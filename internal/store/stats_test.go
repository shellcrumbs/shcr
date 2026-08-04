package store

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
)

func statsByCommand(t *testing.T, s *Store) map[string]CommandStat {
	t.Helper()
	all, err := s.CommandStats(0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]CommandStat, len(all))
	for _, st := range all {
		out[st.Command] = st
	}
	return out
}

func TestStatsCountExecutionsAndOutcomes(t *testing.T) {
	s := newTestStore(t)
	at := int64(1_700_000_000_000)
	hour := int64(time.Hour / time.Millisecond)

	// Same command three times: two clean, one that could not be found.
	for i, exit := range []int{0, 0, 127} {
		id := fmt.Sprintf("c%d", i)
		mustAppend(t, s, startEvent(t, id, "make deploy", at+int64(i)*hour))
		mustAppend(t, s, endEvent(t, id, exit, at+int64(i)*hour+500))
	}
	// One Ctrl-C, which is neither success nor failure.
	mustAppend(t, s, startEvent(t, "c9", "tail -f log", at+4*hour))
	mustAppend(t, s, endEvent(t, "c9", 130, at+4*hour+500))
	// One still running.
	mustAppend(t, s, startEvent(t, "c10", "npm run dev", at+5*hour))

	if _, err := s.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	got := statsByCommand(t, s)

	deploy := got["make deploy"]
	if deploy.Runs != 3 || deploy.Succeeded != 2 || deploy.NeverRan != 1 {
		t.Errorf("make deploy: %+v", deploy)
	}
	if deploy.Failed != 0 {
		t.Errorf("exit 127 should count as never-ran, not failed: %+v", deploy)
	}
	if tail := got["tail -f log"]; tail.Interrupted != 1 || tail.Failed != 0 {
		t.Errorf("Ctrl-C should be interrupted, not failed: %+v", tail)
	}
	if dev := got["npm run dev"]; dev.Unfinished != 1 || dev.Succeeded != 0 {
		t.Errorf("a running command has no outcome yet: %+v", dev)
	}
}

// The refresh recomputes each changed command from all of its executions rather
// than adjusting counters in place, which is the property that makes it
// impossible to double count or drift. This is the test that says so.
func TestIncrementalRefreshMatchesAFullRebuild(t *testing.T) {
	s := newTestStore(t)
	at := int64(1_700_000_000_000)
	minute := int64(60_000)

	commands := []string{"git status", "npm run build", "make deploy", "git status", "ls -al"}
	step := 0
	appendRun := func(text string, exit int, gap int64) {
		id := fmt.Sprintf("c%d", step)
		step++
		when := at + int64(step)*gap
		mustAppend(t, s, startEvent(t, id, text, when))
		mustAppend(t, s, endEvent(t, id, exit, when+100))
	}

	// Refresh repeatedly as work arrives, the way the daemon will.
	for round := range 6 {
		for i, text := range commands {
			appendRun(text, i%3, 7*minute)
		}
		w, err := s.StatsWatermark()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RefreshCommandStats(w); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	incremental := statsByCommand(t, s)

	// Now throw it away and rebuild from nothing.
	if _, err := s.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	full := statsByCommand(t, s)

	if len(incremental) != len(full) {
		t.Fatalf("incremental has %d commands, rebuild has %d", len(incremental), len(full))
	}
	for cmd, want := range full {
		got, ok := incremental[cmd]
		if !ok {
			t.Errorf("%q missing from the incremental cache", cmd)
			continue
		}
		if got.Runs != want.Runs || got.LastTime != want.LastTime ||
			got.Succeeded != want.Succeeded || got.Failed != want.Failed ||
			got.NeverRan != want.NeverRan || got.Interrupted != want.Interrupted {
			t.Errorf("%q counts differ:\n incremental %+v\n rebuild     %+v", cmd, got, want)
		}
		if math.Abs(got.Frecency.Value(at)-want.Frecency.Value(at)) > 1e-9 {
			t.Errorf("%q frecency differs: %.6f vs %.6f",
				cmd, got.Frecency.Value(at), want.Frecency.Value(at))
		}
	}
}

// A command whose every execution has been redacted is no longer a command.
func TestRedactedCommandsLeaveTheCache(t *testing.T) {
	s := newTestStore(t)
	at := int64(1_700_000_000_000)
	mustAppend(t, s, startEvent(t, "c1", "export TOKEN=hunter2", at))
	mustAppend(t, s, endEvent(t, "c1", 0, at+10))
	if _, err := s.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	if _, ok := statsByCommand(t, s)["export TOKEN=hunter2"]; !ok {
		t.Fatal("setup: the command should be in the cache")
	}

	mustAppend(t, s, mkEvent(t, "c1", event.TypeRedact, map[string]any{}, at+20))
	if _, err := s.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	if _, ok := statsByCommand(t, s)["export TOKEN=hunter2"]; ok {
		t.Error("a redacted command is still in the ranking cache")
	}
}

// The whole reason this table exists: deriving it per keystroke measured 815ms
// at 500k executions, because the cost is the scan and not the result.
func TestStatsRefreshAndReadAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	if raceEnabled {
		t.Skip("timing budgets are not meaningful under -race")
	}
	s := newTestStore(t)
	const n = 200_000
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO commands
		(id, command, hostname, device_id, session_id, cwd, shell, start_time,
		 end_time, exit_code, duration_ms, status, pgid, is_background)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0)`)
	if err != nil {
		t.Fatal(err)
	}
	verbs := []string{"git commit -m", "npm run build", "go test ./...", "kubectl get pods",
		"docker compose up", "make deploy", "vim internal/store/store.go", "ls -al"}
	for i := range n {
		text := fmt.Sprintf("%s %d", verbs[i%len(verbs)], i%2000)
		start := int64(1_700_000_000_000 + i*1000)
		if _, err := stmt.Exec(fmt.Sprintf("cmd-%d", i), text, "host-a", "device-a", "sess",
			"/home/u", "bash", start, start+42, 0, 42, StatusCompleted, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	changed, err := s.RefreshCommandStats(0)
	if err != nil {
		t.Fatal(err)
	}
	rebuild := time.Since(started)
	t.Logf("full rebuild of %d distinct commands from %d executions: %v", changed, n, rebuild)

	// The read is what sits between keystrokes, and it is the one with a budget.
	var best time.Duration
	var stats []CommandStat
	for i := range 3 {
		st := time.Now()
		stats, err = s.CommandStats(20000)
		if err != nil {
			t.Fatal(err)
		}
		if e := time.Since(st); i == 0 || e < best {
			best = e
		}
	}
	t.Logf("read of %d cached commands: %v", len(stats), best)
	if best > 50*time.Millisecond {
		t.Errorf("reading the ranking cache took %v, budget is 50ms", best)
	}

	// And the incremental case: one new command since the watermark.
	w, err := s.StatsWatermark()
	if err != nil {
		t.Fatal(err)
	}
	var incremental time.Duration
	for i := range 5 {
		mustAppend(t, s, startEvent(t, fmt.Sprintf("fresh%d", i), fmt.Sprintf("echo hello %d", i),
			int64(1_700_000_000_000+n*1000+int64(i+1)*5000)))
		started = time.Now()
		if _, err := s.RefreshCommandStats(w); err != nil {
			t.Fatal(err)
		}
		took := time.Since(started)
		t.Logf("  incremental refresh %d: %v", i, took)
		if w, err = s.StatsWatermark(); err != nil {
			t.Fatal(err)
		}
		incremental = took
	}
	if incremental > 100*time.Millisecond {
		t.Errorf("incremental refresh took %v, which is not incremental", incremental)
	}
}
