package store

import (
	"path/filepath"
	"testing"
)

func branchStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "branch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Standing outside a repository, nothing shares a branch with you — there is no
// branch to share. Answering yes gave every command that never ran on one a
// multiplier over every command that did, which is backwards: it ranked the
// commands with less context above the ones with more.
func TestNoBranchMeansNoBranchMatch(t *testing.T) {
	s := branchStore(t)
	main := "main"
	for _, c := range []Command{
		{ID: "c1", Command: "ls -al", Cwd: "/tmp", Hostname: "h", SessionID: "s",
			StartTime: 1000, Status: StatusCompleted},
		{ID: "c2", Command: "git push", Cwd: "/home/u/app", Hostname: "h", SessionID: "s",
			GitBranch: &main, StartTime: 2000, Status: StatusCompleted},
	} {
		insertCommandForTest(t, s, c)
	}

	// Outside a repository: Branch is empty.
	got, err := s.CommandContexts([]string{"ls -al", "git push"}, Where{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if got["ls -al"].SameBranch {
		t.Error("a command with no branch matched an empty branch; there is no branch to match")
	}
	if got["git push"].SameBranch {
		t.Error("a command on main matched an empty branch")
	}

	// Inside one, the real comparison still has to work.
	got, err = s.CommandContexts([]string{"ls -al", "git push"}, Where{Hostname: "h", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["git push"].SameBranch {
		t.Error("a command on main did not match branch main")
	}
	if got["ls -al"].SameBranch {
		t.Error("a command with no branch matched branch main")
	}
}

// insertCommandForTest writes a commands row directly. The context query reads
// that table, and going through the event log would only add noise here.
func insertCommandForTest(t *testing.T, s *Store, c Command) {
	t.Helper()
	_, err := s.DB().Exec(`INSERT INTO commands
		(id, command, hostname, device_id, session_id, cwd, git_branch, shell,
		 start_time, status, pgid, is_background, imported)
		VALUES (?,?,?,?,?,?,?,?,?,?,0,0,0)`,
		c.ID, c.Command, c.Hostname, "dev", c.SessionID, c.Cwd, c.GitBranch, "bash",
		c.StartTime, c.Status)
	if err != nil {
		t.Fatal(err)
	}
}

// The same reasoning as the branch, for the other three. An imported command
// carries no directory and no session, so with nothing to compare against they
// would all match each other and collect a multiplier for it.
func TestAnEmptyContextMatchesNothing(t *testing.T) {
	s := branchStore(t)
	insertCommandForTest(t, s, Command{ID: "c1", Command: "ls -al",
		Cwd: "", Hostname: "", SessionID: "", StartTime: 1000, Status: StatusCompleted})

	got, err := s.CommandContexts([]string{"ls -al"}, Where{})
	if err != nil {
		t.Fatal(err)
	}
	c := got["ls -al"]
	if c.SameDir || c.SameHost || c.SameSession || c.SameBranch || c.SameRepo {
		t.Errorf("%+v: nothing was asked about, so nothing should match", c)
	}
}
