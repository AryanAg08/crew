package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sidebar runs in the left pane of every claude session and lists all sessions,
// so you can jump between them without leaving claude.

type sideItem struct {
	project string
	task    *Task // nil = "+ new session" in project
}

type sidebar struct {
	current string // task id of the session this sidebar lives in
	st      *State
	live    map[string]bool
	items   []sideItem
	cursor  int
	msg     string
	busy    bool
	confirm *Task                    // session awaiting y/n to delete
	dirty   bool                     // it has uncommitted changes
	changes map[string]changeSummary // task id → files changed vs its base
	ticks   int
	width   int
	height  int
}

type changesMsg map[string]changeSummary

// loadChanges diffs every session against its base, in the background. It
// skips the work while this sidebar's session isn't on screen.
func (s sidebar) loadChanges() tea.Cmd {
	st, self := s.st, s.sessionName()
	return func() tea.Msg {
		if out, _ := run("", "tmux", "display-message", "-p", "-t", self, "#{session_attached}"); out == "0" {
			return nil
		}
		res := changesMsg{}
		for _, t := range st.Tasks {
			if p := st.project(t.Project); p != nil {
				res[t.ID] = loadBranchChanges(*p, t).summary()
			}
		}
		return res
	}
}

type sideDone struct {
	err    error
	target *Task
}

var currentBar = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render("▌")

func newSidebar(current string) sidebar {
	s := sidebar{current: current}
	s.refresh()
	for i, it := range s.items {
		if it.task != nil && it.task.ID == current {
			s.cursor = i
		}
	}
	return s
}

func (s *sidebar) refresh() {
	if st, err := loadState(); err == nil {
		s.st = st
	} else if s.st == nil {
		s.st = &State{}
	}
	s.live = liveSessions()
	s.items = nil
	for _, p := range s.st.Projects {
		for _, t := range s.st.tasksOf(p.Name) {
			t := t
			s.items = append(s.items, sideItem{project: p.Name, task: &t})
		}
		s.items = append(s.items, sideItem{project: p.Name})
	}
	s.cursor = clamp(s.cursor, len(s.items)-1)
}

func (s sidebar) currentProject() string {
	for _, t := range s.st.Tasks {
		if t.ID == s.current {
			return t.Project
		}
	}
	return ""
}

func (s sidebar) Init() tea.Cmd { return tea.Batch(tick(), s.loadChanges()) }

func (s sidebar) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
	case tickMsg:
		s.refresh()
		if s.ticks++; s.ticks%3 == 0 {
			return s, tea.Batch(tick(), s.loadChanges())
		}
		return s, tick()
	case changesMsg:
		s.changes = msg
	case sideDone:
		s.busy = false
		s.refresh()
		if msg.err != nil {
			s.msg = msg.err.Error()
			return s, nil
		}
		s.msg = ""
		if msg.target != nil {
			switchTo(*msg.target, false)
		}
	case tea.MouseMsg:
		if s.busy || s.confirm != nil || msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
			return s, nil
		}
		_, lineOf := s.render()
		for i, it := range s.items {
			rows := 2 // a session takes two lines, plus one for its changes button
			if it.task == nil {
				rows = 1 // "+ new session"
			} else if s.changes[it.task.ID].any() {
				rows = 3
				if msg.Y == lineOf[i]+2 {
					openChanges(*it.task)
					return s, nil
				}
			}
			if msg.Y >= lineOf[i] && msg.Y < lineOf[i]+rows {
				s.cursor = i
				if it.task == nil {
					return s, nil // clicks only select; enter or n creates a session
				}
				return s.activate()
			}
		}
	case tea.FocusMsg:
		// Arriving here (tab, or a switch from another sidebar): start on this session.
		s.msg = ""
		for i, it := range s.items {
			if it.task != nil && it.task.ID == s.current {
				s.cursor = i
			}
		}
	case tea.KeyMsg:
		if s.busy {
			return s, nil
		}
		if s.confirm != nil {
			t := *s.confirm
			s.confirm = nil
			if k := msg.String(); k == "y" || k == "Y" {
				return s.delete(t)
			}
			s.msg = ""
			return s, nil
		}
		switch msg.String() {
		case "x", "d", "delete":
			if len(s.items) == 0 {
				return s, nil
			}
			if it := s.items[s.cursor]; it.task != nil {
				t := *it.task
				s.confirm, s.dirty, s.msg = &t, isDirty(t), ""
			}
		case "up", "k":
			s.move(-1)
		case "down", "j":
			s.move(1)
		case "enter", "l", "right":
			return s.activate()
		case "c":
			if len(s.items) > 0 && s.items[s.cursor].task != nil {
				openChanges(*s.items[s.cursor].task)
			}
		case "n":
			if p := s.currentProject(); p != "" {
				return s.newSession(p)
			}
		case "esc":
			focusClaude(s.sessionName())
		}
	}
	return s, nil
}

// activate switches to the highlighted session, or creates one for "+ new session".
func (s sidebar) activate() (tea.Model, tea.Cmd) {
	if len(s.items) == 0 {
		return s, nil
	}
	it := s.items[s.cursor]
	if it.task == nil {
		return s.newSession(it.project)
	}
	t := *it.task
	if t.ID == s.current {
		focusClaude(t.Session)
		return s, nil
	}
	s.busy = true
	return s, func() tea.Msg {
		if !s.live[t.Session] {
			p := s.st.project(t.Project)
			if p == nil {
				return sideDone{err: fmt.Errorf("project %s not found", t.Project)}
			}
			if err := resumeTask(*p, t); err != nil {
				return sideDone{err: err}
			}
		}
		ensureSidebar(t)
		return sideDone{target: &t}
	}
}

// move steps the cursor and switches to running sessions as it lands on them,
// keeping focus in the sidebar so ↑/↓ keep browsing.
func (s *sidebar) move(d int) {
	s.cursor = clamp(s.cursor+d, len(s.items)-1)
	s.msg = ""
	it := s.items[s.cursor]
	if it.task == nil || it.task.ID == s.current {
		return
	}
	if !s.live[it.task.Session] {
		s.msg = "stopped · enter to restart"
		return
	}
	ensureSidebar(*it.task)
	switchTo(*it.task, true)
}

// delete removes a session. If it's the one we're in, move to another first.
func (s sidebar) delete(t Task) (tea.Model, tea.Cmd) {
	s.busy, s.msg = true, "deleting…"
	var other *Task
	if t.ID == s.current {
		for _, it := range s.items {
			if it.task != nil && it.task.ID != t.ID && s.live[it.task.Session] {
				o := *it.task
				other = &o
				break
			}
		}
	}
	p := s.st.project(t.Project)
	return s, func() tea.Msg {
		if other != nil {
			ensureSidebar(*other)
			switchTo(*other, true)
		}
		if err := deleteTask(p, t); err != nil {
			return sideDone{err: err}
		}
		return sideDone{}
	}
}

func (s sidebar) sessionName() string {
	for _, t := range s.st.Tasks {
		if t.ID == s.current {
			return t.Session
		}
	}
	return ""
}

func (s sidebar) newSession(project string) (tea.Model, tea.Cmd) {
	p := s.st.project(project)
	if p == nil {
		return s, nil
	}
	proj := *p
	s.busy, s.msg = true, "creating session…"
	return s, func() tea.Msg {
		t, err := createTask(proj)
		if err != nil {
			return sideDone{err: err}
		}
		if _, err := updateState(func(st *State) { st.Tasks = append(st.Tasks, t) }); err != nil {
			return sideDone{err: err}
		}
		return sideDone{target: &t}
	}
}

func (s sidebar) View() string {
	lines, _ := s.render()
	return strings.Join(lines, "\n")
}

// render lays out the pane; lineOf is each item's first line, for mouse clicks.
func (s sidebar) render() (lines []string, lineOf []int) {
	w := s.width
	if w <= 0 {
		w = 34
	}
	add := func(l string) { lines = append(lines, l) }
	pad := func(l string) string {
		if p := w - lipgloss.Width(l); p > 0 {
			l += strings.Repeat(" ", p)
		}
		return l
	}

	add(titleStyle.Render(" crew") + dimStyle.Render(" sessions"))
	lineOf = make([]int, len(s.items))
	lastProject := ""
	for i, it := range s.items {
		if it.project != lastProject {
			add("")
			add(" " + projectStyle.Render(truncate(it.project, w-2)))
			lastProject = it.project
		}
		lineOf[i] = len(lines)
		sel := i == s.cursor
		if it.task == nil {
			l := pad("   " + okStyle.Render("+ new session"))
			if sel {
				l = selStyle.Render(l)
			}
			add(l)
			continue
		}
		t := *it.task
		st := statusOf(t, s.live)
		style, ok := statusStyles[st.State]
		if !ok {
			style = dimStyle
		}
		bar := " "
		if t.ID == s.current {
			bar = currentBar
		}
		br := taskBranch(t)
		if br == "" {
			br = "no branch yet"
		}
		l1 := pad(bar + " " + style.Render(statusIcons[st.State]) + " " + truncate(sessionLabel(t), w-5))
		l2 := pad(bar + "   " + dimStyle.Render(truncate("⎇ "+br, w-15)) + " " + style.Render(st.State))
		if sel {
			l1, l2 = selStyle.Render(l1), selStyle.Render(l2)
		}
		add(l1)
		add(l2)
		if c := s.changes[t.ID]; c.any() {
			add(pad(bar + "   " + btnStyle.Render(" "+truncate(c.buttonLabel(), w-6)+" ")))
		}
	}

	// Footer pinned to the bottom of the pane.
	footer := []string{}
	if s.confirm != nil {
		warn := statusStyles["needs-input"]
		footer = append(footer, " "+warn.Render("Delete "+truncate(sessionLabel(*s.confirm), w-12)+"?"))
		if s.dirty {
			footer = append(footer, " "+errStyle.Render("uncommitted changes will be lost"))
		}
		footer = append(footer, " "+warn.Render("y delete · any key cancel"))
	} else if s.msg != "" {
		footer = append(footer, " "+statusStyles["needs-input"].Render(truncate(s.msg, w-2)))
	}
	footer = append(footer, dimStyle.Render(truncate(" ↑↓ switch · n new · x del · c diff", w)),
		dimStyle.Render(truncate(" tab ⇄ claude · ctrl-b d exit", w)))
	if gap := s.height - len(lines) - len(footer); gap > 0 {
		lines = append(lines, make([]string, gap)...)
	}
	return append(lines, footer...), lineOf
}
