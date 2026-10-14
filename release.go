package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// While crew runs, each session's worktree holds its branch, and git refuses to
// check that branch out anywhere else. When crew exits it stops the sessions
// and detaches their worktrees, freeing the branches for any editor; opening a
// session again checks its branch back out. The worktree folder, including any
// uncommitted changes, is left in place.

// taskBranch is the session's branch, whether checked out or released.
func taskBranch(t Task) string {
	if b := currentBranch(t.Worktree); b != "" {
		return b
	}
	return t.Branch
}

// releaseTask stops the session and detaches its worktree from its branch.
func releaseTask(t Task) (Task, error) {
	run("", "tmux", "kill-session", "-t", t.Session)
	br := currentBranch(t.Worktree)
	if br == "" {
		return t, nil // no branch yet, or already released
	}
	if _, err := run(t.Worktree, "git", "switch", "--detach", "--quiet"); err != nil {
		return t, err
	}
	t.Branch = br
	return t, nil
}

var usedBy = regexp.MustCompile(`(?:checked out|used by worktree) at '([^']+)'`)

// reattach checks a released session's branch back out in its worktree.
func reattach(t Task) error {
	if t.Branch == "" || currentBranch(t.Worktree) != "" {
		return nil
	}
	if _, err := run(t.Worktree, "git", "switch", "--quiet", t.Branch); err != nil {
		if m := usedBy.FindStringSubmatch(err.Error()); m != nil {
			return fmt.Errorf("%s is checked out in %s; switch that checkout to another branch, then open this session again",
				t.Branch, strings.Replace(m[1], home, "~", 1))
		}
		return fmt.Errorf("couldn't check out %s: %v", t.Branch, err)
	}
	_, err := updateState(func(st *State) {
		for i := range st.Tasks {
			if st.Tasks[i].ID == t.ID {
				st.Tasks[i].Branch = ""
			}
		}
	})
	return err
}

// releaseAll releases every session and returns lines describing what happened.
func releaseAll() []string {
	st, err := loadState()
	if err != nil {
		return []string{"crew: " + err.Error()}
	}
	var lines []string
	for i, t := range st.Tasks {
		had := currentBranch(t.Worktree)
		rt, err := releaseTask(t)
		if err != nil {
			lines = append(lines, fmt.Sprintf("  %s/%s: couldn't release: %v", t.Project, had, err))
			continue
		}
		st.Tasks[i] = rt
		if had == "" {
			continue
		}
		line := fmt.Sprintf("  %-16s %s", t.Project, had)
		if isDirty(rt) {
			line += "  (uncommitted changes kept in " + strings.Replace(t.Worktree, home, "~", 1) + ")"
		}
		lines = append(lines, line)
	}
	if err := st.save(); err != nil {
		lines = append(lines, "crew: saving state: "+err.Error())
	}
	return lines
}

// Each running crew screen leaves a pid file, so quitting one of several open
// crew screens doesn't stop sessions another one is using.
func instanceFile() string {
	return filepath.Join(crewDir, "instances", strconv.Itoa(os.Getpid()))
}

func registerInstance() {
	os.MkdirAll(filepath.Dir(instanceFile()), 0o755)
	os.WriteFile(instanceFile(), nil, 0o644)
}

// otherInstances reports whether another crew screen is still running.
func otherInstances() bool {
	entries, _ := os.ReadDir(filepath.Dir(instanceFile()))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if syscall.Kill(pid, 0) == nil {
			return true
		}
		os.Remove(filepath.Join(filepath.Dir(instanceFile()), e.Name())) // stale
	}
	return false
}
