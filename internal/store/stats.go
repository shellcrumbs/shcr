package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/shellcrumbs/shcr/internal/rank"
)

// CommandStat is one distinct command with everything ranking needs that does
// not depend on where the user is standing.
type CommandStat struct {
	Command   string
	Runs      int
	LastTime  int64
	Frecency  rank.Counter
	Succeeded int
	Failed    int
	NeverRan  int
	// Interrupted covers Ctrl-C and friends: neither success nor failure, and
	// excluded from the failure rate rather than counted against the command.
	Interrupted int
	// Unfinished is running and orphaned executions — no result to judge.
	Unfinished   int
	ImportedRuns int
	LastFailedAt int64
}

// RefreshCommandStats brings the ranking cache up to date.
//
// from is a position in the event log, not a point in time. That distinction
// matters: events do not arrive in the order they happened. A command pulled
// from a peer can carry last week's timestamp, and a watermark on start_time
// would step straight over it — the command would never enter the cache at
// all. The event log's rowid only ever goes up, whoever produced the event.
//
// Each changed command is recomputed from all of its executions rather than
// adjusted. That is what makes this correct without transition bookkeeping:
// nothing can double count, nothing drifts, and a redaction that rewrites a
// command's text repairs itself on the next pass.
//
// Pass 0 to rebuild everything. It returns the number of commands touched and
// the new watermark.
func (s *Store) RefreshCommandStats(from int64) (int, int64, error) {
	// Fix the upper bound first, so events landing while this runs are left for
	// the next pass rather than skipped by a watermark that ran ahead of them.
	var upTo int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM events`).Scan(&upTo); err != nil {
		return 0, from, err
	}

	var (
		rows *sql.Rows
		err  error
	)
	if from <= 0 {
		if _, err := s.db.Exec(`DELETE FROM command_stats`); err != nil {
			return 0, from, err
		}
		rows, err = s.db.Query(`SELECT DISTINCT command FROM commands WHERE command <> ''`)
	} else {
		// INDEXED BY is doing real work on the equivalent commands query: left
		// to choose, SQLite scans an index end to end when it satisfies a
		// DISTINCT, ignoring a far more selective bound. Here the events rowid
		// is the primary key, so the range is already the cheap path.
		rows, err = s.db.Query(`
			SELECT DISTINCT c.command
			  FROM events e JOIN commands c ON c.id = e.command_id
			 WHERE e.rowid > ? AND e.rowid <= ? AND c.command <> ''`, from, upTo)
	}
	if err != nil {
		return 0, from, err
	}
	var changed []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, from, err
		}
		changed = append(changed, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, from, err
	}
	if len(changed) == 0 {
		return 0, upTo, s.setStatsWatermark(upTo)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, from, err
	}
	defer tx.Rollback()

	upsert, err := tx.Prepare(`
		INSERT INTO command_stats
		    (command, runs, last_time, weight, weight_at, succeeded, failed,
		     never_ran, interrupted, unfinished, imported_runs, last_failed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(command) DO UPDATE SET
		    runs=excluded.runs, last_time=excluded.last_time,
		    weight=excluded.weight, weight_at=excluded.weight_at,
		    succeeded=excluded.succeeded, failed=excluded.failed,
		    never_ran=excluded.never_ran, interrupted=excluded.interrupted,
		    unfinished=excluded.unfinished, imported_runs=excluded.imported_runs,
		    last_failed_at=excluded.last_failed_at`)
	if err != nil {
		return 0, from, err
	}
	defer upsert.Close()

	for _, cmd := range changed {
		st, err := statFor(tx, cmd)
		if err != nil {
			return 0, from, err
		}
		if st.Runs == 0 {
			// Every execution of it was redacted away.
			if _, err := tx.Exec(`DELETE FROM command_stats WHERE command = ?`, cmd); err != nil {
				return 0, from, err
			}
			continue
		}
		if _, err := upsert.Exec(cmd, st.Runs, st.LastTime,
			st.Frecency.Weight, st.Frecency.At, st.Succeeded, st.Failed, st.NeverRan,
			st.Interrupted, st.Unfinished, st.ImportedRuns, st.LastFailedAt); err != nil {
			return 0, from, err
		}
	}
	if _, err := tx.Exec(`UPDATE stats_meta SET watermark = ? WHERE id = 1`, upTo); err != nil {
		return 0, from, err
	}
	return len(changed), upTo, tx.Commit()
}

func (s *Store) setStatsWatermark(w int64) error {
	_, err := s.db.Exec(`UPDATE stats_meta SET watermark = ? WHERE id = 1`, w)
	return err
}

// statFor recomputes one command's statistics from all of its executions.
func statFor(tx *sql.Tx, command string) (CommandStat, error) {
	st := CommandStat{Command: command}
	rows, err := tx.Query(
		`SELECT start_time, exit_code, status, imported FROM commands
		  WHERE command = ? ORDER BY start_time ASC`, command)
	if err != nil {
		return st, err
	}
	defer rows.Close()

	for rows.Next() {
		var start int64
		var exit sql.NullInt64
		var status string
		var imported int
		if err := rows.Scan(&start, &exit, &status, &imported); err != nil {
			return st, err
		}
		st.Runs++
		if start > st.LastTime {
			st.LastTime = start
		}
		// Ascending order matters: the counter's burst suppression asks how long
		// it has been since the last execution it counted.
		st.Frecency.Observe(start)
		if imported != 0 {
			st.ImportedRuns++
		}
		switch {
		case status == StatusRunning || status == StatusOrphaned:
			st.Unfinished++
		case !exit.Valid:
			st.Unfinished++
		default:
			switch rank.ClassifyExit(int(exit.Int64)) {
			case rank.OutcomeSuccess:
				st.Succeeded++
			case rank.OutcomeNeverRan:
				st.NeverRan++
			case rank.OutcomeInterrupted:
				st.Interrupted++
			default:
				st.Failed++
				if start > st.LastFailedAt {
					st.LastFailedAt = start
				}
			}
		}
	}
	return st, rows.Err()
}

// StatsWatermark is how far through the event log the ranking cache has been
// refreshed — a rowid, not a time.
func (s *Store) StatsWatermark() (int64, error) {
	var w int64
	err := s.db.QueryRow(`SELECT watermark FROM stats_meta WHERE id = 1`).Scan(&w)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return w, err
}

// CommandStats returns the ranking cache, most recently used first.
//
// The limit bounds how much history the picker is willing to consider matching
// against, which is what keeps a keystroke's work independent of how long the
// history has been accumulating.
func (s *Store) CommandStats(limit int) ([]CommandStat, error) {
	if limit <= 0 {
		limit = 20000
	}
	rows, err := s.db.Query(`
		SELECT command, runs, last_time, weight, weight_at, succeeded, failed,
		       never_ran, interrupted, unfinished, imported_runs, last_failed_at
		  FROM command_stats ORDER BY last_time DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CommandStat
	for rows.Next() {
		var st CommandStat
		if err := rows.Scan(&st.Command, &st.Runs, &st.LastTime,
			&st.Frecency.Weight, &st.Frecency.At, &st.Succeeded, &st.Failed,
			&st.NeverRan, &st.Interrupted, &st.Unfinished, &st.ImportedRuns,
			&st.LastFailedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// CommandStatFor returns the cached statistics for one command's text.
//
// A deduplicated row stands for every execution of that command, so this is
// what tells the reader whether they are looking at one run or fifty — and it
// is what makes the ordering explainable rather than merely confident.
func (s *Store) CommandStatFor(command string) (CommandStat, bool, error) {
	var st CommandStat
	err := s.db.QueryRow(`
		SELECT command, runs, last_time, weight, weight_at, succeeded, failed,
		       never_ran, interrupted, unfinished, imported_runs, last_failed_at
		  FROM command_stats WHERE command = ?`, command).
		Scan(&st.Command, &st.Runs, &st.LastTime, &st.Frecency.Weight, &st.Frecency.At,
			&st.Succeeded, &st.Failed, &st.NeverRan, &st.Interrupted, &st.Unfinished,
			&st.ImportedRuns, &st.LastFailedAt)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	return st, err == nil, err
}

// Summary describes a command's history in a few words: how many times it has
// run and how those runs turned out. Empty when it has only ever run once,
// because "ran 1 time" is not worth a line.
func (st CommandStat) Summary() string {
	if st.Runs <= 1 {
		return ""
	}
	out := fmt.Sprintf("ran %d×", st.Runs)
	var parts []string
	if st.Succeeded > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", st.Succeeded))
	}
	if st.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", st.Failed))
	}
	if st.NeverRan > 0 {
		parts = append(parts, fmt.Sprintf("%d not found", st.NeverRan))
	}
	if st.Interrupted > 0 {
		parts = append(parts, fmt.Sprintf("%d interrupted", st.Interrupted))
	}
	if st.Unfinished > 0 {
		parts = append(parts, fmt.Sprintf("%d unfinished", st.Unfinished))
	}
	if len(parts) > 0 {
		out += " · " + strings.Join(parts, ", ")
	}
	return out
}
