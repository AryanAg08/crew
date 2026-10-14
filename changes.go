package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// A session's branch changes, split by how far along they are:
//
//	base ──(pushed)── pushedTip ──(committed)── HEAD ──(to be added)── working tree
//
// base is where the branch left the project's base branch; pushedTip is the
// last commit the remote has.

const (
	secAdd = iota
	secCommitted
	secPushed
)

// Committed means committed but not pushed yet.
var sectionNames = [...]string{"To be added", "Committed", "Pushed"}

type fileChange struct {
	Path      string
	Add, Del  int // -1 for binary files
	Untracked bool
	Section   int
	Commit    string // set when the change is one commit's, shown per commit
}

type commit struct {
	Short, Subject string
	Files          []fileChange
}

type section struct {
	Files    []fileChange
	Commits  int
	Log      []commit // per-commit files, newest first (Committed only)
	from, to string   // diff range; to == "" means the working tree
}

type branchChanges struct {
	Sections [3]section
}

// changeSummary is what the sidebar button shows: files per section.
type changeSummary [3]int

func (c changeSummary) any() bool { return c[0]+c[1]+c[2] > 0 }

// changeBase is the commit the session's work is compared against.
func changeBase(p Project, t Task) string {
	if mb, err := run(t.Worktree, "git", "merge-base", "HEAD", p.Base); err == nil {
		return mb
	}
	return "HEAD"
}

// upstream is the remote branch this branch pushes to, if it has been pushed.
func upstream(t Task) string {
	if u, err := run(t.Worktree, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		return u
	}
	if br := taskBranch(t); br != "" {
		if _, err := run(t.Worktree, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+br); err == nil {
			return "origin/" + br
		}
	}
	return ""
}

func numstat(dir string, section int, args ...string) []fileChange {
	out, err := run(dir, "git", append([]string{"diff", "--numstat", "--no-renames"}, args...)...)
	if err != nil {
		return nil
	}
	return parseNumstat(out, section)
}

func parseNumstat(out string, section int) []fileChange {
	var files []fileChange
	for _, l := range strings.Split(out, "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		add, errA := strconv.Atoi(parts[0])
		del, errD := strconv.Atoi(parts[1])
		if errA != nil || errD != nil {
			add, del = -1, -1
		}
		files = append(files, fileChange{Path: parts[2], Add: add, Del: del, Section: section})
	}
	return files
}

func commitCount(dir, from, to string) int {
	out, _ := run(dir, "git", "rev-list", "--count", from+".."+to)
	n, _ := strconv.Atoi(out)
	return n
}

func loadBranchChanges(p Project, t Task) branchChanges {
	dir := t.Worktree
	base := changeBase(p, t)
	pushedTip := base
	if u := upstream(t); u != "" {
		if mb, err := run(dir, "git", "merge-base", "HEAD", u); err == nil {
			pushedTip = mb
		}
	}

	var c branchChanges
	add := &c.Sections[secAdd]
	add.from = "HEAD"
	add.Files = numstat(dir, secAdd, "HEAD")
	untracked, _ := run(dir, "git", "ls-files", "--others", "--exclude-standard")
	for _, f := range strings.Split(untracked, "\n") {
		if f == "" {
			continue
		}
		n := 0
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			n = strings.Count(string(b), "\n")
		}
		add.Files = append(add.Files, fileChange{Path: f, Add: n, Untracked: true, Section: secAdd})
	}

	if pushedTip != "HEAD" {
		cm := &c.Sections[secCommitted]
		cm.from, cm.to = pushedTip, "HEAD"
		cm.Files = numstat(dir, secCommitted, pushedTip, "HEAD")
		cm.Commits = commitCount(dir, pushedTip, "HEAD")
		log, _ := run(dir, "git", "log", "--format=%H%x09%h%x09%s", pushedTip+"..HEAD")
		for _, l := range strings.Split(log, "\n") {
			parts := strings.SplitN(l, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			out, _ := run(dir, "git", "show", "--numstat", "--format=", "--no-renames", parts[0])
			files := parseNumstat(out, secCommitted)
			for i := range files {
				files[i].Commit = parts[0]
			}
			cm.Log = append(cm.Log, commit{Short: parts[1], Subject: parts[2], Files: files})
		}
	}
	if pushedTip != base {
		ps := &c.Sections[secPushed]
		ps.from, ps.to = base, pushedTip
		ps.Files = numstat(dir, secPushed, base, pushedTip)
		ps.Commits = commitCount(dir, base, pushedTip)
	}
	return c
}

func (c branchChanges) summary() changeSummary {
	var s changeSummary
	for i, sec := range c.Sections {
		s[i] = len(sec.Files)
	}
	return s
}

func (c branchChanges) fileDiff(dir string, f fileChange) string {
	var cmd *exec.Cmd
	sec := c.Sections[f.Section]
	switch {
	case f.Commit != "":
		cmd = exec.Command("git", "show", "--color=always", "--format=", f.Commit, "--", f.Path)
	case f.Untracked:
		cmd = exec.Command("git", "diff", "--no-index", "--color=always", "/dev/null", f.Path)
	case sec.to == "":
		cmd = exec.Command("git", "diff", "--color=always", sec.from, "--", f.Path)
	default:
		cmd = exec.Command("git", "diff", "--color=always", sec.from, sec.to, "--", f.Path)
	}
	cmd.Dir = dir
	out, _ := cmd.Output() // --no-index exits 1 when files differ
	if len(out) == 0 {
		return dimStyle.Render("(no textual diff)")
	}
	return string(out)
}

// buttonLabel is the sidebar's one-line summary, e.g. "add 2 · commit 1 · pushed 3".
func (s changeSummary) buttonLabel() string {
	var parts []string
	for i, label := range []string{"add", "commit", "pushed"} {
		if s[i] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", label, s[i]))
		}
	}
	return strings.Join(parts, " · ")
}

// openChanges shows the changes viewer in a popup over the current tmux client.
func openChanges(t Task) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	go exec.Command("tmux", "display-popup", "-E", "-w", "92%", "-h", "90%",
		"-T", " changes ", "--", self, "--changes", t.ID).Run()
}

// listRow is a line in the viewer's left column: a section header or a file.
type listRow struct {
	header string
	commit *commit // a commit's header line inside Committed
	file   *fileChange
}

// changesView is the popup: changed files by section on the left, the diff on the right.
type changesView struct {
	p      Project
	t      Task
	ch     branchChanges
	rows   []listRow
	cursor int // index into rows; always a file row when there are files
	diff   viewport.Model
	width  int
	height int
}

func newChangesView(id string) (changesView, error) {
	st, err := loadState()
	if err != nil {
		return changesView{}, err
	}
	for _, t := range st.Tasks {
		if t.ID != id {
			continue
		}
		p := st.project(t.Project)
		if p == nil {
			break
		}
		c := changesView{p: *p, t: t, diff: viewport.New(80, 20)}
		c.reload("")
		return c, nil
	}
	return changesView{}, fmt.Errorf("session %s not found", id)
}

// reload re-reads the branch's changes, keeping the cursor on keepPath if it's still listed.
func (c *changesView) reload(keepPath string) {
	c.ch = loadBranchChanges(c.p, c.t)
	c.rows = nil
	for i, sec := range c.ch.Sections {
		if len(sec.Files) == 0 {
			continue
		}
		h := sectionNames[i]
		commits := fmt.Sprintf("%d commit", sec.Commits)
		if sec.Commits != 1 {
			commits += "s"
		}
		if i != secAdd {
			h += " · " + commits
		}
		c.rows = append(c.rows, listRow{header: h})
		if i == secCommitted && len(sec.Log) > 0 {
			for k := range c.ch.Sections[i].Log {
				cm := &c.ch.Sections[i].Log[k]
				c.rows = append(c.rows, listRow{commit: cm})
				for j := range cm.Files {
					c.rows = append(c.rows, listRow{file: &cm.Files[j]})
				}
			}
			continue
		}
		for j := range sec.Files {
			c.rows = append(c.rows, listRow{file: &c.ch.Sections[i].Files[j]})
		}
	}
	c.cursor = c.step(-1, 1)
	for i, r := range c.rows {
		if r.file != nil && r.file.Section == secAdd && r.file.Path == keepPath {
			c.cursor = i
		}
	}
}

type editedMsg struct{ path string }

// editable reports whether the highlighted file can be edited: only files
// still to be added; committed and pushed changes are history.
func (c changesView) editable() *fileChange {
	if len(c.rows) == 0 || c.rows[c.cursor].file == nil || c.rows[c.cursor].file.Section != secAdd {
		return nil
	}
	return c.rows[c.cursor].file
}

// step returns the next file row from `from` in direction d, or the current one.
func (c changesView) step(from, d int) int {
	for i := from + d; i >= 0 && i < len(c.rows); i += d {
		if c.rows[i].file != nil {
			return i
		}
	}
	if from < 0 {
		return 0
	}
	return from
}

func (c changesView) Init() tea.Cmd { return nil }

func (c *changesView) listWidth() int { return min(44, max(26, c.width/4)) }

func (c *changesView) loadDiff() {
	if len(c.rows) == 0 || c.rows[c.cursor].file == nil {
		c.diff.SetContent(dimStyle.Render("No changes on this branch."))
		return
	}
	w := max(10, c.width-c.listWidth()-3)
	lines := strings.Split(c.ch.fileDiff(c.t.Worktree, *c.rows[c.cursor].file), "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	c.diff.SetContent(strings.Join(lines, "\n"))
	c.diff.GotoTop()
}

func (c changesView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case editedMsg:
		c.reload(msg.path)
		c.loadDiff()
	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.diff.Width = max(10, c.width-c.listWidth()-3)
		c.diff.Height = max(3, c.height-3)
		c.loadDiff()
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.X < c.listWidth() {
			_, rowAt := c.renderList()
			if y := msg.Y - 2; y >= 0 && y < len(rowAt) && rowAt[y] >= 0 && c.rows[rowAt[y]].file != nil {
				c.cursor = rowAt[y]
				c.loadDiff()
			}
			return c, nil
		}
		var cmd tea.Cmd
		c.diff, cmd = c.diff.Update(msg) // wheel scrolls the diff
		return c, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return c, tea.Quit
		case "up", "k":
			c.cursor = c.step(c.cursor, -1)
			c.loadDiff()
		case "down", "j":
			c.cursor = c.step(c.cursor, 1)
			c.loadDiff()
		case "o", "e", "enter":
			if f := c.editable(); f != nil {
				vi := exec.Command("vi", f.Path)
				vi.Dir = c.t.Worktree
				path := f.Path
				return c, tea.ExecProcess(vi, func(error) tea.Msg { return editedMsg{path} })
			}
		default: // space, pgup/pgdn... scroll the diff
			var cmd tea.Cmd
			c.diff, cmd = c.diff.Update(msg)
			return c, cmd
		}
	}
	return c, nil
}

func (c changesView) View() string {
	br := taskBranch(c.t)
	if br == "" {
		br = "no branch yet"
	}
	header := " " + projectStyle.Render(c.t.Project) + dimStyle.Render(" ⎇ "+br+" · changes since it left "+c.p.Base)

	lw := c.listWidth()
	list, _ := c.renderList()
	h := max(3, c.height-3)
	left := lipgloss.NewStyle().Width(lw).Height(h).MaxHeight(h).Render(strings.Join(list, "\n"))
	sep := dimStyle.Render(strings.TrimSuffix(strings.Repeat("│\n", h), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, sep, " "+strings.ReplaceAll(c.diff.View(), "\n", "\n "))

	footer := " ↑/↓ file · space/pgdn scroll · q close"
	if c.editable() != nil {
		footer = " ↑/↓ file · o edit in vi · space/pgdn scroll · q close"
	}
	return header + "\n\n" + body + "\n" + dimStyle.Render(truncate(footer, c.width))
}

// renderList draws the left column; rowAt maps each line to its row (-1 for spacing).
func (c changesView) renderList() (list []string, rowAt []int) {
	lw := c.listWidth()
	add := func(l string, row int) {
		list = append(list, l)
		rowAt = append(rowAt, row)
	}
	for i, r := range c.rows {
		if r.commit != nil {
			add("  "+commitStyle.Render(r.commit.Short)+" "+truncate(r.commit.Subject, lw-len(r.commit.Short)-4), i)
			continue
		}
		if r.file == nil {
			if i > 0 {
				add("", -1)
			}
			add(" "+sectionStyle.Render(truncate(r.header, lw-2)), i)
			continue
		}
		indent := "   "
		if r.file.Commit != "" {
			indent = "     "
		}
		f := r.file
		stat := addStyle.Render(fmt.Sprintf("+%d", f.Add)) + " " + delStyle.Render(fmt.Sprintf("−%d", f.Del))
		if f.Add < 0 {
			stat = dimStyle.Render("bin")
		} else if f.Untracked {
			stat = addStyle.Render("new")
		}
		name := truncate(f.Path, lw-lipgloss.Width(stat)-len(indent)-2)
		line := indent + name + strings.Repeat(" ", max(1, lw-lipgloss.Width(name)-lipgloss.Width(stat)-len(indent)-1)) + stat + " "
		if i == c.cursor {
			line = selStyle.Render(line)
		}
		add(line, i)
	}
	if len(c.rows) == 0 {
		add(dimStyle.Render(" nothing to show"), -1)
	}
	return list, rowAt
}

var (
	addStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	delStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	btnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("221"))
	commitStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("173"))
)
