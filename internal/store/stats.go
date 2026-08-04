package store

import (
	"database/sql"

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
// Only commands with an execution newer than the watermark are touched, and
// each of those is recomputed from all of its executions rather than adjusted.
// Recomputing is what makes this correct without transition bookkeeping: there
// is no way to double count, no way to drift, and a redaction that rewrites a
// command's text fixes itself on the next pass.
//
// Pass 0 as the watermark to rebuild everything.
func (s *Store) RefreshCommandStats(from int64) (int, error) {
	if from == 0 {
		if _, err := s.db.Exec(`DELETE FROM command_stats`); err != nil {
			return 0, err
		}
	}

	// Which commands have moved.
	//
	// INDEXED BY is doing real work here. Left to choose, SQLite scans
	// idx_commands_command end to end, because that index satisfies the DISTINCT
	// without a temporary B-tree — and ignores the far more selective time
	// bound. That turned "refresh after one new command" into a 95ms full index
	// scan. Forcing the range search and paying for a temp B-tree over the
	// handful of matching rows takes it to under a millisecond. It also means
	// that if the index is ever dropped this fails loudly rather than quietly
	// going back to scanning everything.
	rows, err := s.db.Query(
		`SELECT DISTINCT command FROM commands INDEXED BY idx_commands_start
		  WHERE start_time >= ? AND command <> ''`, from)
	if err != nil {
		return 0, err
	}
	var changed []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, err
		}
		changed = append(changed, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(changed) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
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
		return 0, err
	}
	defer upsert.Close()

	var watermark int64
	for _, cmd := range changed {
		st, err := statFor(tx, cmd)
		if err != nil {
			return 0, err
		}
		if st.Runs == 0 {
			// Every execution of it was redacted away.
			if _, err := tx.Exec(`DELETE FROM command_stats WHERE command = ?`, cmd); err != nil {
				return 0, err
			}
			continue
		}
		if st.LastTime > watermark {
			watermark = st.LastTime
		}
		if _, err := upsert.Exec(cmd, st.Runs, st.LastTime,
			st.Frecency.Weight, st.Frecency.At, st.Succeeded, st.Failed, st.NeverRan,
			st.Interrupted, st.Unfinished, st.ImportedRuns, st.LastFailedAt); err != nil {
			return 0, err
		}
	}

	// One millisecond past the newest execution seen, so the next pass does not
	// redo the whole tail of this one.
	if watermark > 0 {
		if _, err := tx.Exec(`UPDATE stats_meta SET watermark = ? WHERE id = 1`, watermark+1); err != nil {
			return 0, err
		}
	}
	return len(changed), tx.Commit()
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

// StatsWatermark is the point the ranking cache has been refreshed to.
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
