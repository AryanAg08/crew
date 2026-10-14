package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("%s", s)
	}
	return s, nil
}

func expandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// newProject validates a git repo path and detects its name and base branch.
func newProject(input string) (Project, error) {
	path, err := expandPath(input)
	if err != nil {
		return Project{}, err
	}
	top, err := run(path, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return Project{}, fmt.Errorf("not a git repo: %s", path)
	}
	base := "main"
	if ref, err := run(top, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		base = strings.TrimPrefix(ref, "origin/")
	} else if cur, err := run(top, "git", "branch", "--show-current"); err == nil && cur != "" {
		base = cur
	}
	return Project{Name: slug(filepath.Base(top)), Path: top, Base: base}, nil
}

// createTask makes a fresh worktree at the project's base (no branch yet) and
// starts claude there; the user tells claude which branch to create or switch to.
func createTask(p Project) (Task, error) {
	short := strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	id := p.Name + "--" + short
	t := Task{
		ID:        id,
		Project:   p.Name,
		Worktree:  filepath.Join(treesDir, p.Name, short),
		Session:   "crew_" + id,
		ClaudeID:  uuid.NewString(),
		CreatedAt: time.Now(),
	}
	if _, err := run(p.Path, "git", "worktree", "add", "--detach", t.Worktree, p.Base); err != nil {
		return Task{}, err
	}
	copyEnvFiles(p.Path, t.Worktree)
	if err := startSession(t, []string{"--session-id", t.ClaudeID, "--append-system-prompt", branchGuide(p)}); err != nil {
		return Task{}, err
	}
	return t, nil
}

// branchGuide tells claude how this session is set up and how to pick a branch.
func branchGuide(p Project) string {
	return fmt.Sprintf(`You are running inside crew: this session has its own git worktree of %s, separate from the user's main checkout and from other parallel sessions.
It started on a detached HEAD at %s, so no branch is chosen yet. Before changing any files, make sure the session is on the branch the user wants:
- new work: git switch -c <new-branch>
- existing branch: git switch <branch>. If git says it is already checked out in another worktree (often the main checkout), tell the user and suggest a new branch from it instead.
If the user has not said which branch to use, ask them first. Never commit on a detached HEAD.`, p.Name, p.Base)
}

// copyEnvFiles copies untracked top-level .env* files, which a fresh worktree lacks.
func copyEnvFiles(src, dst string) {
	matches, _ := filepath.Glob(filepath.Join(src, ".env*"))
	for _, m := range matches {
		name := filepath.Base(m)
		if _, err := run(src, "git", "ls-files", "--error-unmatch", name); err == nil {
			continue // tracked, already in the worktree
		}
		if fi, err := os.Stat(m); err != nil || fi.IsDir() {
			continue
		}
		in, err := os.Open(m)
		if err != nil {
			continue
		}
		if out, err := os.Create(filepath.Join(dst, name)); err == nil {
			io.Copy(out, in)
			out.Close()
		}
		in.Close()
	}
}

func startSession(t Task, claudeArgs []string) error {
	os.Remove(filepath.Join(statusDir, t.ID))
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	// When claude exits, end the whole session so the sidebar doesn't linger.
	args := []string{"new-session", "-d", "-s", t.Session, "-c", t.Worktree,
		"-e", "CREW_TASK_ID=" + t.ID, "-e", "CREW_SESSION=" + t.Session, "--",
		shell, "-lc", `claude "$@"; tmux kill-session -t "$CREW_SESSION"`, "crew", "--settings", hooksFile}
	args = append(args, claudeArgs...)
	if _, err := run("", "tmux", args...); err != nil {
		return fmt.Errorf("tmux: %v", err)
	}
	run("", "tmux", "set-option", "-t", t.Session, "mouse", "on") // scroll with the wheel
	ensureSidebar(t)
	return nil
}

const sidebarWidth = "34"

// ensureSidebar adds the sessions sidebar to the left of claude if it isn't there.
func ensureSidebar(t Task) {
	installKeys()
	out, err := run("", "tmux", "list-panes", "-t", t.Session, "-F", "#{pane_start_command}")
	if err != nil || strings.Contains(out, "--sidebar") {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// -d keeps focus on claude; -b puts the sidebar on the left.
	run("", "tmux", "split-window", "-d", "-h", "-b", "-l", sidebarWidth, "-t", t.Session, "-c", t.Worktree,
		"--", self, "--sidebar", t.ID)
	// tmux scales panes when the window resizes; keep the sidebar narrow.
	fix := "resize-pane -t " + t.Session + ":.0 -x " + sidebarWidth
	for _, hook := range []string{"client-attached", "client-session-changed", "client-resized", "window-resized"} {
		run("", "tmux", "set-hook", "-t", t.Session, hook, fix)
	}
}

// resumeTask restarts claude for a task whose tmux session has ended.
func resumeTask(p Project, t Task) error {
	if _, err := os.Stat(t.Worktree); err != nil {
		return fmt.Errorf("worktree missing: %s", t.Worktree)
	}
	if err := reattach(t); err != nil {
		return err
	}
	// Resume the newest conversation in this worktree (it changes after /clear).
	resume := []string{"--session-id", t.ClaudeID} // claude exited before saving anything
	if p := transcriptPath(t.Worktree, t.ClaudeID); p != "" && filepath.Dir(p) == claudeDir(t.Worktree) {
		resume = []string{"--resume", strings.TrimSuffix(filepath.Base(p), ".jsonl")}
	}
	return startSession(t, append(resume, "--append-system-prompt", branchGuide(p)))
}

func liveSessions() map[string]bool {
	out, err := run("", "tmux", "list-sessions", "-F", "#{session_name}")
	live := map[string]bool{}
	if err != nil {
		return live
	}
	for _, l := range strings.Split(out, "\n") {
		live[l] = true
	}
	return live
}

type taskStatus struct {
	State string // working | idle | needs-input | starting | stopped
	Since time.Time
}

func statusOf(t Task, live map[string]bool) taskStatus {
	if !live[t.Session] {
		return taskStatus{State: "stopped"}
	}
	f := filepath.Join(statusDir, t.ID)
	b, err := os.ReadFile(f)
	if err != nil {
		return taskStatus{State: "starting"}
	}
	fi, _ := os.Stat(f)
	return taskStatus{State: strings.TrimSpace(string(b)), Since: fi.ModTime()}
}

func isDirty(t Task) bool {
	out, err := run(t.Worktree, "git", "status", "--porcelain")
	return err == nil && out != ""
}

// deleteTask removes the worktree, forgets the task, then ends its tmux session.
// The branch is kept. The session is killed last because a sidebar may be
// deleting its own session, which ends the sidebar's process too.
func deleteTask(p *Project, t Task) error {
	if _, err := os.Stat(t.Worktree); err == nil && p != nil {
		if _, err := run(p.Path, "git", "worktree", "remove", "--force", t.Worktree); err != nil {
			return err
		}
	}
	if p != nil {
		run(p.Path, "git", "worktree", "prune")
	}
	os.Remove(filepath.Join(statusDir, t.ID))
	if _, err := updateState(func(st *State) { st.Tasks = removeTask(st.Tasks, t.ID) }); err != nil {
		return err
	}
	run("", "tmux", "kill-session", "-t", t.Session)
	return nil
}

func attachCmd(t Task) *exec.Cmd {
	ensureSidebar(t)
	if os.Getenv("TMUX") != "" {
		return exec.Command("tmux", "switch-client", "-t", t.Session)
	}
	return exec.Command("tmux", "attach-session", "-t", t.Session)
}

// findRepos lists git repos near where crew runs and near added projects,
// so adding a project is a pick instead of typing a path.
func findRepos(st *State) []string {
	cwd, _ := os.Getwd()
	roots := []string{cwd, filepath.Dir(cwd)}
	for _, p := range st.Projects {
		roots = append(roots, filepath.Dir(p.Path))
	}
	for _, d := range []string{"Documents", "code", "projects", "dev", "src", "repos", "work"} {
		roots = append(roots, filepath.Join(home, d))
	}
	added := map[string]bool{}
	for _, p := range st.Projects {
		added[p.Path] = true
	}
	seen := map[string]bool{}
	var out []string
	consider := func(dir string) {
		if seen[dir] || added[dir] || strings.HasPrefix(dir, crewDir) {
			return
		}
		seen[dir] = true
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			out = append(out, dir)
		}
	}
	for _, r := range roots {
		consider(r)
		entries, _ := os.ReadDir(r)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" {
				consider(filepath.Join(r, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// installKeys makes Tab move focus between the sidebar and claude in crew
// sessions. Elsewhere Tab passes through untouched.
func installKeys() {
	run("", "tmux", "set-option", "-s", "focus-events", "on") // sidebar knows when it gets focus
	run("", "tmux", "unbind-key", "-n", "M-Up")               // shortcuts from an earlier version
	run("", "tmux", "unbind-key", "-n", "M-Down")
	run("", "tmux", "bind-key", "-n", "Tab", "if-shell", "-F", "#{m:crew_*,#{session_name}}",
		"select-pane -t :.+", "send-keys Tab")
}

// switchTo moves the current tmux client to t's session, focusing either its
// sidebar (to keep browsing) or claude.
func switchTo(t Task, sidebarFocus bool) {
	pane := ":.1"
	if sidebarFocus {
		pane = ":.0"
	}
	run("", "tmux", "select-pane", "-t", t.Session+pane)
	run("", "tmux", "switch-client", "-t", t.Session)
}

func focusClaude(session string) {
	if session != "" {
		run("", "tmux", "select-pane", "-t", session+":.1")
	}
}
