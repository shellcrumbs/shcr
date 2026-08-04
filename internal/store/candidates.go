package store

import (
	"strings"
)

// Where is the context a ranked query is asked from: the directory the user is
// standing in, the shell they are standing in, and the machine it is all
// happening on.
type Where struct {
	Cwd       string
	Repo      string
	SessionID string
	Hostname  string
	Branch    string
}

// CommandContext answers, for one command, whether *any* of its executions
// happened where the user is now.
//
// Any rather than the most recent: the command you ran in this directory last
// week should still beat one you ran somewhere else yesterday, and asking only
// about the latest execution loses that.
type CommandContext struct {
	SameDir     bool
	SameRepo    bool
	SameSession bool
	SameHost    bool
	SameBranch  bool
}

// CommandContexts computes the context signals for a bounded set of commands.
//
// Bounded is the point. This is the second half of a two-phase search: the
// first phase scores every cached command on match and frecency alone, which
// needs no database at all, and only the handful that survive get this far.
func (s *Store) CommandContexts(commands []string, w Where) (map[string]CommandContext, error) {
	out := make(map[string]CommandContext, len(commands))
	if len(commands) == 0 {
		return out, nil
	}
	args := []any{w.Cwd, w.Hostname, w.SessionID, w.Branch}
	repoPrefix := ""
	if w.Repo != "" {
		repoPrefix = strings.TrimSuffix(w.Repo, "/") + "/"
	}
	args = append(args, repoPrefix, repoPrefix)
	for _, c := range commands {
		args = append(args, c)
	}

	rows, err := s.db.Query(`
		SELECT command,
		       MAX(cwd = ?), MAX(hostname = ?), MAX(session_id = ?),
		       MAX(COALESCE(git_branch, '') = ?),
		       MAX(? <> '' AND (cwd || '/') LIKE ? || '%')
		  FROM commands
		 WHERE command IN (`+placeholders(len(commands))+`)
		 GROUP BY command`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var cmd string
		var dir, host, session, branch, repo int
		if err := rows.Scan(&cmd, &dir, &host, &session, &branch, &repo); err != nil {
			return nil, err
		}
		out[cmd] = CommandContext{
			SameDir:     dir != 0,
			SameHost:    host != 0,
			SameSession: session != 0,
			SameBranch:  branch != 0,
			SameRepo:    repo != 0,
		}
	}
	return out, rows.Err()
}

// LatestExecutions returns the most recent execution of each named command,
// which is the row the picker draws: its status, duration, exit code and where
// it ran.
//
// Joined against the ranking cache rather than grouped, because the cache
// already knows when each command last ran and the join then lands on the
// command index directly.
func (s *Store) LatestExecutions(commands []string) (map[string]Command, error) {
	out := make(map[string]Command, len(commands))
	if len(commands) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(commands))
	for _, c := range commands {
		args = append(args, c)
	}
	rows, err := s.db.Query(`
		SELECT `+prefixed(commandColumns, "c.")+`
		  FROM command_stats s
		  JOIN commands c ON c.command = s.command AND c.start_time = s.last_time
		 WHERE s.command IN (`+placeholders(len(commands))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		// Two executions can share a millisecond; either will do, they are the
		// same command text at the same moment.
		if _, seen := out[c.Command]; !seen {
			out[c.Command] = c
		}
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// prefixed qualifies a bare column list for a join, so commandColumns stays the
// single place the column order is written down.
func prefixed(columns, prefix string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(strings.ReplaceAll(p, "\n", " "))
	}
	return strings.Join(parts, ", ")
}
