package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Base string `json:"base"`
}

type Task struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Worktree  string    `json:"worktree"`
	Session   string    `json:"session"`          // tmux session name
	ClaudeID  string    `json:"claude_id"`        // claude --session-id, used to resume
	Branch    string    `json:"branch,omitempty"` // released branch, checked out again on open
	CreatedAt time.Time `json:"created_at"`
}

type State struct {
	Projects []Project `json:"projects"`
	Tasks    []Task    `json:"tasks"`
}

var (
	home, _   = os.UserHomeDir()
	crewDir   = crewHome()
	stateFile = filepath.Join(crewDir, "state.json")
	statusDir = filepath.Join(crewDir, "status")
	hooksFile = filepath.Join(crewDir, "hooks.json")
	treesDir  = filepath.Join(crewDir, "worktrees")
)

// crewHome is ~/.crew, or $CREW_HOME when set (used for isolated testing).
func crewHome() string {
	if d := os.Getenv("CREW_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".crew")
}

func loadState() (*State, error) {
	s := &State{}
	b, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, s)
}

func (s *State) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, stateFile)
}

// updateState applies fn to the latest state on disk and saves it, so the main
// screen and the sidebars (separate processes) don't overwrite each other.
func updateState(fn func(*State)) (*State, error) {
	s, err := loadState()
	if err != nil {
		return nil, err
	}
	fn(s)
	return s, s.save()
}

func (s *State) project(name string) *Project {
	for i := range s.Projects {
		if s.Projects[i].Name == name {
			return &s.Projects[i]
		}
	}
	return nil
}

func (s *State) tasksOf(project string) []Task {
	var out []Task
	for _, t := range s.Tasks {
		if t.Project == project {
			out = append(out, t)
		}
	}
	return out
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// slug makes a string safe for directory and tmux session names.
func slug(s string) string {
	return strings.Trim(unsafeChars.ReplaceAllString(s, "-"), "-")
}

// Hooks run inside every crew-launched claude and write the task's status file.
// CREW_TASK_ID is set on the tmux session, so hooks are no-ops elsewhere.
func writeHooks() error {
	set := func(status string) []map[string]any {
		cmd := `[ -n "$CREW_TASK_ID" ] && printf ` + status + ` > '` + statusDir + `/'"$CREW_TASK_ID"`
		if status == "needs-input" {
			cmd += `; osascript -e "display notification \"$CREW_TASK_ID needs input\" with title \"crew\"" >/dev/null 2>&1 || true`
		}
		return []map[string]any{{"matcher": "", "hooks": []map[string]any{{"type": "command", "command": cmd}}}}
	}
	hooks := map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": set("working"),
		"PreToolUse":       set("working"),
		"Stop":             set("idle"),
		"Notification":     set("needs-input"),
	}}
	b, _ := json.MarshalIndent(hooks, "", "  ")
	return os.WriteFile(hooksFile, b, 0o644)
}

func ensureDirs() error {
	for _, d := range []string{crewDir, statusDir, treesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return writeHooks()
}
