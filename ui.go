package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	screenHome    screen = iota // list of projects
	screenProject               // one project's sessions + history
)

type mode int

const (
	modeList mode = iota
	modeAddProject
	modeConfirm
	modeBusy
	modePick
)

const otherPath = "Other path…"

type model struct {
	st      *State
	live    map[string]bool
	screen  screen
	mode    mode
	homeCur int
	proj    string // project shown on screenProject
	projCur int    // 0 = "+ New session", i = tasks[i-1]
	input   textinput.Model
	confirm string // question shown in modeConfirm
	onYes   func() tea.Msg
	msg     string
	isErr   bool
	width   int
	height  int

	// picker (modePick)
	pickTitle  string
	pickItems  []string
	pickCursor int
	onPick     func(m *model, item string) tea.Cmd
}

type tickMsg time.Time
type doneMsg struct {
	text  string
	err   error
	apply func(*State) // state change to make on the UI goroutine
	open  *Task        // attach to this task once applied
}

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	projectStyle = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selStyle     = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	youStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	claudeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("173")).Bold(true)
	costStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	statusStyles = map[string]lipgloss.Style{
		"working":     lipgloss.NewStyle().Foreground(lipgloss.Color("39")),
		"idle":        lipgloss.NewStyle().Foreground(lipgloss.Color("114")),
		"needs-input": lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true),
		"starting":    lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		"stopped":     lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
	}
	statusIcons = map[string]string{
		"working": "●", "idle": "○", "needs-input": "◆", "starting": "◌", "stopped": "·",
	}
	statusOrder = []string{"needs-input", "working", "idle", "starting", "stopped"}
)

func newModel(st *State) model {
	ti := textinput.New()
	ti.CharLimit = 500
	m := model{st: st, input: ti}
	m.refresh()
	return m
}

func (m *model) refresh() {
	if st, err := loadState(); err == nil {
		m.st = st // pick up sessions created from a sidebar
	}
	m.live = liveSessions()
	m.homeCur = clamp(m.homeCur, len(m.st.Projects)-1)
	if m.screen == screenProject {
		if m.st.project(m.proj) == nil {
			m.screen = screenHome
		}
		m.projCur = clamp(m.projCur, len(m.st.tasksOf(m.proj)))
	}
}

func clamp(v, max int) int {
	if v > max {
		v = max
	}
	if v < 0 {
		v = 0
	}
	return v
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tick() }

func (m *model) flash(text string, err error) {
	if err != nil {
		m.msg, m.isErr = err.Error(), true
	} else {
		m.msg, m.isErr = text, false
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.refresh()
		return m, tick()
	case doneMsg:
		m.mode = modeList
		if msg.err == nil && msg.apply != nil {
			if st, err := updateState(msg.apply); err != nil {
				msg.err = err
			} else {
				m.st = st
			}
		}
		m.flash(msg.text, msg.err)
		m.refresh()
		if msg.err == nil && msg.open != nil {
			return m.open(*msg.open)
		}
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case modeConfirm:
			if k := msg.String(); k == "y" || k == "Y" {
				m.mode = modeBusy
				return m, m.onYes
			}
			m.mode, m.msg = modeList, ""
			return m, nil
		case modeBusy:
			return m, nil
		case modePick:
			return m.updatePick(msg)
		case modeAddProject:
			return m.updateInput(msg)
		}
		if m.screen == screenHome {
			return m.updateHome(msg)
		}
		return m.updateProject(msg)
	}
	return m, nil
}

func (m model) updateHome(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c":
		return m.quit()
	case "up", "k":
		m.homeCur = clamp(m.homeCur-1, len(m.st.Projects)-1)
	case "down", "j":
		m.homeCur = clamp(m.homeCur+1, len(m.st.Projects)-1)
	case "p":
		return m, m.pickRepo()
	case "enter", "right", "l":
		if len(m.st.Projects) == 0 {
			return m, m.pickRepo()
		}
		m.screen, m.proj, m.projCur, m.msg = screenProject, m.st.Projects[m.homeCur].Name, 0, ""
	case "x", "d":
		if len(m.st.Projects) == 0 {
			return m, nil
		}
		name := m.st.Projects[m.homeCur].Name
		if n := len(m.st.tasksOf(name)); n > 0 {
			m.flash("", fmt.Errorf("%s still has %d session(s); delete them first", name, n))
			return m, nil
		}
		m.confirm, m.mode = fmt.Sprintf("Remove project %s from crew? The repo itself is untouched. (y/n)", name), modeConfirm
		m.onYes = func() tea.Msg {
			return doneMsg{text: "removed " + name, apply: func(st *State) {
				for i, p := range st.Projects {
					if p.Name == name {
						st.Projects = append(st.Projects[:i], st.Projects[i+1:]...)
						break
					}
				}
			}}
		}
	}
	return m, nil
}

func (m model) updateProject(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	tasks := m.st.tasksOf(m.proj)
	switch k.String() {
	case "q", "ctrl+c":
		return m.quit()
	case "esc", "left", "h", "backspace":
		m.screen, m.msg = screenHome, ""
	case "up", "k":
		m.projCur = clamp(m.projCur-1, len(tasks))
	case "down", "j":
		m.projCur = clamp(m.projCur+1, len(tasks))
	case "n":
		return m.newSession()
	case "enter", "right", "l":
		if m.projCur == 0 {
			return m.newSession()
		}
		return m.open(tasks[m.projCur-1])
	case "x", "d":
		if m.projCur == 0 {
			return m, nil
		}
		t := tasks[m.projCur-1]
		label := truncate(sessionLabel(t), 50)
		q := fmt.Sprintf("Delete session %q? Its worktree is removed; branches are kept. (y/n)", label)
		if isDirty(t) {
			q = fmt.Sprintf("%q has UNCOMMITTED changes that will be lost. Delete anyway? (y/n)", label)
		}
		m.confirm, m.mode = q, modeConfirm
		p := m.st.project(t.Project)
		m.onYes = func() tea.Msg {
			if err := deleteTask(p, t); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: "deleted " + label}
		}
	}
	return m, nil
}

// quit asks first when sessions are running: quitting stops them and frees
// their branches (unless another crew screen is still open).
func (m model) quit() (tea.Model, tea.Cmd) {
	running := 0
	for _, t := range m.st.Tasks {
		if m.live[t.Session] {
			running++
		}
	}
	if running == 0 || otherInstances() {
		return m, tea.Quit
	}
	what := fmt.Sprintf("%d running sessions and frees their branches", running)
	if running == 1 {
		what = "1 running session and frees its branch"
	}
	m.confirm = "Quit crew? This stops " + what + " for your editor. (y/n)"
	m.mode, m.onYes = modeConfirm, tea.Quit
	return m, nil
}

func (m model) newSession() (tea.Model, tea.Cmd) {
	p := *m.st.project(m.proj)
	m.mode, m.isErr = modeBusy, false
	m.msg = fmt.Sprintf("creating a fresh copy of %s from %s…", p.Name, p.Base)
	return m, func() tea.Msg {
		t, err := createTask(p)
		if err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{apply: func(st *State) { st.Tasks = append(st.Tasks, t) }, open: &t}
	}
}

func (m model) open(t Task) (tea.Model, tea.Cmd) {
	if !m.live[t.Session] {
		p := m.st.project(t.Project)
		if p == nil {
			m.flash("", fmt.Errorf("project %s not found", t.Project))
			return m, nil
		}
		if err := resumeTask(*p, t); err != nil {
			m.flash("", err)
			return m, nil
		}
	}
	m.msg = ""
	return m, tea.ExecProcess(attachCmd(t), func(err error) tea.Msg { return doneMsg{err: err} })
}

// pickRepo offers git repos found nearby, plus typing a path by hand.
func (m *model) pickRepo() tea.Cmd {
	var items []string
	for _, r := range findRepos(m.st) {
		items = append(items, strings.Replace(r, home, "~", 1))
	}
	if len(items) == 0 {
		return m.startPathInput()
	}
	items = append(items, otherPath)
	m.mode, m.msg = modePick, ""
	m.pickTitle, m.pickItems, m.pickCursor = "Add which project?", items, 0
	m.onPick = func(m *model, item string) tea.Cmd {
		if item == otherPath {
			return m.startPathInput()
		}
		m.addProject(item)
		return nil
	}
	return nil
}

func (m *model) startPathInput() tea.Cmd {
	m.mode, m.msg = modeAddProject, ""
	m.input.Placeholder = "path to a git repo, e.g. ~/code/my-app"
	m.input.SetValue("")
	return m.input.Focus()
}

func (m model) updatePick(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q", "ctrl+c":
		m.mode = modeList
	case "up", "k":
		m.pickCursor = clamp(m.pickCursor-1, len(m.pickItems)-1)
	case "down", "j":
		m.pickCursor = clamp(m.pickCursor+1, len(m.pickItems)-1)
	case "enter":
		m.mode = modeList
		return m, m.onPick(&m, m.pickItems[m.pickCursor])
	}
	return m, nil
}

func (m model) updateInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "ctrl+c":
		m.mode = modeList
		m.input.Blur()
		return m, nil
	case "enter":
		m.addProject(strings.TrimSpace(m.input.Value()))
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m *model) addProject(path string) {
	p, err := newProject(path)
	if err == nil && m.st.project(p.Name) != nil {
		err = fmt.Errorf("project %s already added", p.Name)
	}
	if err != nil {
		m.flash("", err)
		return
	}
	st, err := updateState(func(st *State) { st.Projects = append(st.Projects, p) })
	if err != nil {
		m.flash("", err)
		return
	}
	m.st, m.mode = st, modeList
	m.homeCur = len(m.st.Projects) - 1
	m.flash(fmt.Sprintf("added %s (base: %s) — enter to open it", p.Name, p.Base), nil)
	m.refresh()
}

func removeTask(ts []Task, id string) []Task {
	var out []Task
	for _, t := range ts {
		if t.ID != id {
			out = append(out, t)
		}
	}
	return out
}

// sessionLabel is the session's title, or its first message, for display.
func sessionLabel(t Task) string {
	h := loadHistory(t)
	if h.Title != "" {
		return h.Title
	}
	for _, msg := range h.Msgs {
		if msg.Role == "you" {
			return msg.Text
		}
	}
	return "new session"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// shortPath shows a path with ~ for home, keeping its tail when it's long.
func shortPath(p string, n int) string {
	p = strings.Replace(p, home, "~", 1)
	if r := []rune(p); len(r) > n {
		return "…" + string(r[len(r)-n+1:])
	}
	return p
}

// usageCols is a fixed-width "tokens  cost" pair for list rows.
func usageCols(u Usage) string {
	if u.Tokens() == 0 {
		return dimStyle.Render(fmt.Sprintf("%7s  %7s", "–", ""))
	}
	return fmt.Sprintf("%7s  %s", fmtTokens(u.Tokens()), costStyle.Render(fmt.Sprintf("%7s", fmtCost(u.Cost))))
}

// truncate shortens s to n characters on one line.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (m model) w() int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

func (m model) selected(line string) string {
	if pad := m.w() - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return selStyle.Render(line)
}

func (m model) View() string {
	var b strings.Builder
	if m.screen == screenHome {
		m.viewHome(&b)
	} else {
		m.viewProject(&b)
	}

	b.WriteString("\n")
	switch m.mode {
	case modePick:
		b.WriteString(" " + projectStyle.Render(m.pickTitle) + "\n")
		for i, it := range m.pickItems {
			if i == m.pickCursor {
				b.WriteString(selStyle.Render(" ❯ "+it+" ") + "\n")
			} else {
				b.WriteString("   " + it + "\n")
			}
		}
	case modeAddProject:
		b.WriteString(" Add project\n " + m.input.View() + "\n")
	case modeConfirm:
		b.WriteString(" " + errStyle.Render(m.confirm) + "\n")
	}
	if m.msg != "" {
		if m.isErr {
			b.WriteString(" " + errStyle.Render(m.msg) + "\n")
		} else {
			b.WriteString(" " + okStyle.Render(m.msg) + "\n")
		}
	}

	var help string
	switch {
	case m.mode == modeAddProject:
		help = "enter confirm · esc cancel"
	case m.mode == modePick:
		help = "↑/↓ choose · enter select · esc cancel"
	case m.screen == screenHome:
		help = "enter open project · p add project · x remove · q quit   (cost = estimate at API prices)"
	default:
		help = "enter open · n new session · x delete · esc back · q quit   (in claude: ctrl-b d to come back)"
	}
	b.WriteString("\n" + dimStyle.Render(" "+help) + "\n")
	return b.String()
}

func (m model) viewHome(b *strings.Builder) {
	var all Usage
	for _, p := range m.st.Projects {
		all.add(projectUsage(m.st, p.Name))
	}
	b.WriteString(titleStyle.Render(" crew") + dimStyle.Render("  parallel claude sessions"))
	if all.Tokens() > 0 {
		b.WriteString(dimStyle.Render("   total ") + fmtTokens(all.Tokens()) + dimStyle.Render(" tokens · ") + costStyle.Render(fmtCost(all.Cost)))
	}
	b.WriteString("\n\n")
	if len(m.st.Projects) == 0 {
		b.WriteString(dimStyle.Render("  No projects yet. Press p to add a git repo.") + "\n")
		return
	}
	nameW := 0
	for _, p := range m.st.Projects {
		nameW = max(nameW, len(p.Name))
	}
	for i, p := range m.st.Projects {
		counts := map[string]int{}
		for _, t := range m.st.tasksOf(p.Name) {
			counts[statusOf(t, m.live).State]++
		}
		var badges []string
		for _, s := range statusOrder {
			if counts[s] > 0 {
				badges = append(badges, statusStyles[s].Render(fmt.Sprintf("%s %d %s", statusIcons[s], counts[s], s)))
			}
		}
		summary := dimStyle.Render("no sessions")
		if len(badges) > 0 {
			summary = strings.Join(badges, "  ")
		}
		line := fmt.Sprintf(" %s  %s  %s  %s", projectStyle.Render(fmt.Sprintf("%-*s", nameW, p.Name)),
			dimStyle.Render(fmt.Sprintf("%-40s", shortPath(p.Path, 40))), usageCols(projectUsage(m.st, p.Name)), summary)
		if i == m.homeCur {
			line = m.selected(line)
		}
		b.WriteString(line + "\n")
	}
}

func (m model) viewProject(b *strings.Builder) {
	p := m.st.project(m.proj)
	if p == nil {
		return
	}
	pu := projectUsage(m.st, p.Name)
	b.WriteString(titleStyle.Render(" crew") + dimStyle.Render(" › ") + projectStyle.Render(p.Name) +
		dimStyle.Render("  "+shortPath(p.Path, 50)+"  ⎇ "+p.Base+"   total ") + fmtTokens(pu.Tokens()) +
		dimStyle.Render(" tokens · ") + costStyle.Render(fmtCost(pu.Cost)) + "\n\n")

	tasks := m.st.tasksOf(p.Name)
	line := "   " + okStyle.Render("+ New session")
	if m.projCur == 0 {
		line = m.selected(line)
	}
	b.WriteString(line + "\n")

	branchW := len("no branch yet")
	for _, t := range tasks {
		branchW = max(branchW, len(taskBranch(t)))
	}
	branchW = min(branchW, 30)
	for i, t := range tasks {
		s := statusOf(t, m.live)
		st, ok := statusStyles[s.State]
		if !ok {
			st = dimStyle
		}
		br := taskBranch(t)
		brText := fmt.Sprintf("%-*s", branchW, truncate(br, branchW))
		if br == "" {
			brText = dimStyle.Render(fmt.Sprintf("%-*s", branchW, "no branch yet"))
		}
		right := st.Render(fmt.Sprintf("%-11s", s.State)) + " " + dimStyle.Render(fmt.Sprintf("%3s", ago(s.Since)))
		titleW := max(10, m.w()-branchW-24-20)
		line := fmt.Sprintf("   %s %s  %-*s  %s  %s", st.Render(statusIcons[s.State]), brText, titleW, truncate(sessionLabel(t), titleW),
			usageCols(taskUsage(t)), right)
		if m.projCur == i+1 {
			line = m.selected(line)
		}
		b.WriteString(line + "\n")
	}

	// History of the highlighted session.
	b.WriteString("\n" + dimStyle.Render(" ─ History "+strings.Repeat("─", max(0, m.w()-12))) + "\n")
	if m.projCur == 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Opens Claude in a fresh copy of %s (from %s), separate from your other sessions.\n  Tell Claude which branch to create, or which existing branch to work on.", p.Name, p.Base)) + "\n")
		return
	}
	u := taskUsage(tasks[m.projCur-1])
	b.WriteString(fmt.Sprintf("  %s %s  %s %s  %s %s  %s %s  %s\n",
		dimStyle.Render("input"), fmtTokens(u.Input), dimStyle.Render("output"), fmtTokens(u.Output),
		dimStyle.Render("cache write"), fmtTokens(u.CacheWrite), dimStyle.Render("cache read"), fmtTokens(u.CacheRead),
		costStyle.Render(fmtCost(u.Cost))))
	h := loadHistory(tasks[m.projCur-1])
	if len(h.Msgs) == 0 {
		b.WriteString(dimStyle.Render("  No messages yet.") + "\n")
		return
	}
	// Leave room for the header, list, and footer.
	room := 12
	if m.height > 0 {
		room = max(3, m.height-(len(tasks)+12))
	}
	msgs := h.Msgs
	if len(msgs) > room {
		msgs = msgs[len(msgs)-room:]
	}
	for _, msg := range msgs {
		who := youStyle.Render("  you    ")
		if msg.Role == "claude" {
			who = claudeStyle.Render("  claude ")
		}
		b.WriteString(who + truncate(msg.Text, m.w()-11) + "\n")
	}
}
