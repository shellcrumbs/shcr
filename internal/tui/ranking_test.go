package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/rank"
	"github.com/shellcrumbs/shcr/internal/store"
)

func rankStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "rank.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// record writes one execution the way the daemon would.
func record(t *testing.T, st *store.Store, id, command, cwd, host, session string, at int64, exit int) {
	t.Helper()
	payload, err := json.Marshal(event.StartPayload{
		Command: command, Hostname: host, SessionID: session, Cwd: cwd,
		Shell: "bash", StartTime: at, PGID: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(event.Event{
		EventID: id + "-s", CommandID: id, DeviceID: "dev", Type: event.TypeStart,
		Payload: payload, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	end, err := json.Marshal(event.EndPayload{EndTime: at + 100, ExitCode: exit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(event.Event{
		EventID: id + "-e", CommandID: id, DeviceID: "dev", Type: event.TypeEnd,
		Payload: end, CreatedAt: at + 100,
	}); err != nil {
		t.Fatal(err)
	}
}

// The claim the whole scheme rests on: within a tier, a command you actually
// use here beats one that matches the letters better and nothing else.
func TestRankingPrefersTheCommandYouUseHere(t *testing.T) {
	st := rankStore(t)
	now := int64(1_800_000_000_000)
	hour := int64(time.Hour / time.Millisecond)
	day := 24 * hour

	for i := range 8 {
		record(t, st, fmt.Sprintf("used%d", i), "npm run build",
			"/home/u/app", "laptop", "s1", now-int64(i+1)*hour, 0)
	}
	// Matches "bld" better — b and l consecutive from position 0 — but ran once,
	// six months ago, somewhere else, in another shell.
	record(t, st, "stranger", "blade-runner.sh --scan", "/var/tmp", "server", "s9", now-180*day, 0)

	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	stats, err := st.CommandStats(0)
	if err != nil {
		t.Fatal(err)
	}

	// The premise: the stranger really is the better match.
	tokens := rank.Tokens("bld")
	used, _ := rank.MatchCommand("npm run build", tokens)
	stranger, _ := rank.MatchCommand("blade-runner.sh --scan", tokens)
	if stranger.Tier != used.Tier || stranger.Score <= used.Score {
		t.Fatalf("premise gone: stranger %+v, used %+v", stranger, used)
	}

	where := store.Where{Cwd: "/home/u/app", Hostname: "laptop", SessionID: "s1"}
	got, err := rankedResults(st, stats, where, "bld", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want both commands: %+v", len(got), got)
	}
	if got[0].Command != "npm run build" {
		t.Errorf("ranked %q first; the command used here should win", got[0].Command)
	}
}

// Eight executions of one command are one row, not eight.
func TestResultsAreDeduplicated(t *testing.T) {
	st := rankStore(t)
	now := int64(1_800_000_000_000)
	for i := range 8 {
		record(t, st, fmt.Sprintf("r%d", i), "git status", "/home/u", "laptop", "s1",
			now-int64(i+1)*60_000, 0)
	}
	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.CommandStats(0)

	for _, query := range []string{"", "git"} {
		got, err := rankedResults(st, stats, store.Where{}, query, "", now)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("query %q returned %d rows for one command", query, len(got))
		}
	}
}

// With nothing typed the picker shows history in the order it happened. Ranking
// an empty query by use would put this morning's habit above the command just
// run, which is not what pressing Ctrl+R means.
func TestEmptyQueryIsMostRecentFirst(t *testing.T) {
	st := rankStore(t)
	now := int64(1_800_000_000_000)
	minute := int64(60_000)

	// Used far more often, but not recently.
	for i := range 10 {
		record(t, st, fmt.Sprintf("old%d", i), "make deploy", "/home/u", "laptop", "s1",
			now-int64(i+20)*minute, 0)
	}
	record(t, st, "fresh", "echo hello", "/home/u", "laptop", "s1", now-minute, 0)

	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.CommandStats(0)

	got, err := rankedResults(st, stats, store.Where{}, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Command != "echo hello" {
		t.Errorf("empty query should open on the most recent command, got %+v", got)
	}
}

func TestStatusFilterSurvivesRanking(t *testing.T) {
	st := rankStore(t)
	now := int64(1_800_000_000_000)
	record(t, st, "ok", "npm run build", "/home/u", "laptop", "s1", now-3*60_000, 0)
	record(t, st, "bad", "npm run test", "/home/u", "laptop", "s1", now-2*60_000, 1)
	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.CommandStats(0)

	got, err := rankedResults(st, stats, store.Where{}, "npm", store.StatusFailed, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "npm run test" {
		t.Errorf("status filter did not survive ranking: %+v", got)
	}
}

// Ranking runs between keystrokes, over every command in the cache.
func TestRankingCostPerKeystroke(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	if raceEnabled {
		t.Skip("timing budgets are not meaningful under -race")
	}
	st := rankStore(t)
	now := int64(1_800_000_000_000)
	record(t, st, "real", "npm run build", "/home/u/app", "laptop", "s1", now-60_000, 0)
	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}

	// A full cache of commands that mostly do not match, which is the expensive
	// case: every one of them is matched before anything is ruled out.
	verbs := []string{"git commit -m", "kubectl get pods", "docker compose up",
		"terraform apply", "cargo build --release", "make deploy"}
	stats := make([]store.CommandStat, 0, 20000)
	for i := range 20000 {
		stats = append(stats, store.CommandStat{
			Command:   fmt.Sprintf("%s --flag-%d value-%d", verbs[i%len(verbs)], i, i*7),
			Runs:      1,
			LastTime:  now - int64(i)*1000,
			Succeeded: 1,
			Frecency:  rank.Counter{Weight: 1, At: now - int64(i)*1000},
		})
	}
	real, _ := st.CommandStats(0)
	stats = append(stats, real...)

	where := store.Where{Cwd: "/home/u/app", Hostname: "laptop", SessionID: "s1"}
	var best time.Duration
	for i, q := range []string{"npm", "npm b", "npm bu", "npm bui", "kubectl pods", "zzz"} {
		started := time.Now()
		if _, err := rankedResults(st, stats, where, q, "", now); err != nil {
			t.Fatal(err)
		}
		if e := time.Since(started); i == 0 || e < best {
			best = e
		}
	}
	t.Logf("ranking %d cached commands: best keystroke %v", len(stats), best)
	// Well inside the gap between keystrokes; a person typing fast leaves 80ms.
	if best > 25*time.Millisecond {
		t.Errorf("a keystroke costs %v, budget is 25ms", best)
	}
}
