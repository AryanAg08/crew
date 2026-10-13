package main

import (
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// version is set at build time: -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("crew " + version)
		return
	}
	for _, bin := range []string{"git", "tmux", "claude"} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(os.Stderr, "crew: %s not found in PATH\n", bin)
			os.Exit(1)
		}
	}
	if err := ensureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "crew:", err)
		os.Exit(1)
	}
	st, err := loadState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crew: reading state:", err)
		os.Exit(1)
	}
	var prog *tea.Program
	if len(os.Args) == 3 && os.Args[1] == "--changes" { // popup opened from the sidebar
		cv, err := newChangesView(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "crew:", err)
			os.Exit(1)
		}
		prog = tea.NewProgram(cv, tea.WithAltScreen(), tea.WithMouseCellMotion())
	} else if len(os.Args) == 3 && os.Args[1] == "--sidebar" { // runs inside each claude session's left pane
		prog = tea.NewProgram(newSidebar(os.Args[2]), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus())
	} else {
		prog = tea.NewProgram(newModel(st), tea.WithAltScreen())
	}
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "crew:", err)
		os.Exit(1)
	}
}
