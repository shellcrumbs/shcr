// Package gitinfo resolves the current branch by reading .git directly.
//
// This deliberately avoids shelling out to git: branch lookup happens in the
// background sender rather than the shell hook, and a fork per prompt is
// exactly the cost this project promised not to pay.
package gitinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// Branch walks up from dir looking for a repository and returns its checked-out
// branch, or a short commit id when detached. It returns "" when dir is not in
// a repository, and never returns an error — branch is best-effort metadata.
func Branch(dir string) string {
	gitDir := findGitDir(dir)
	if gitDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(head) >= 7 { // detached HEAD
		return head[:7]
	}
	return ""
}

func findGitDir(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		switch {
		case err == nil && info.IsDir():
			return candidate
		case err == nil:
			// A .git file points elsewhere (submodule or linked worktree).
			if b, err := os.ReadFile(candidate); err == nil {
				if p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: "); ok {
					if !filepath.IsAbs(p) {
						p = filepath.Join(dir, p)
					}
					return p
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
