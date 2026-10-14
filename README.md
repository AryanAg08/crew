# crew

Run many Claude Code sessions in parallel — across branches and across projects —
from one terminal screen.

Each session gets its own git worktree, so sessions never step on each other or
on your main checkout. Type `crew`, pick a project, start a session, and tell
Claude which branch to create or work on.

## Install

```bash
brew install AryanAg08/tap/crew
```

Needs macOS, [Claude Code](https://claude.com/claude-code) (`claude` on your PATH),
and git. Homebrew installs tmux for you.

To build from source instead: `go install github.com/AryanAg08/crew@latest` (Go 1.23+).

## Use

```bash
crew
```

**Home** lists your projects, with live session counts and token usage.

| Key | Action |
|---|---|
| `p` | add a project (picks from git repos it finds, or type a path) |
| `enter` | open a project |
| `x` | remove a project from crew (the repo is untouched) |

**A project** lists its sessions — branch, title, status, tokens and cost — with
the highlighted session's conversation underneath.

| Key | Action |
|---|---|
| `enter` on **+ New session** | fresh worktree, opens Claude; tell it which branch to use |
| `enter` on a session | open it (restarts it if it had stopped) |
| `x` | delete a session — removes its worktree, keeps the branch |
| `esc` | back |

**Inside a session** a sidebar on the left lists every session.

| Key | Action |
|---|---|
| `tab` | move between the sidebar and Claude |
| `↑` / `↓` in the sidebar | switch to the previous / next session |
| click a session | switch to it |
| `n` | new session in this project |
| `x` | delete the highlighted session |
| `c` or the changes button | view the branch's changes |
| `ctrl-b d` | back to the crew home screen |

The **changes** view splits a branch's work into *To be added* (uncommitted and
new files — press `o` to edit one in vi), *Committed* (grouped by commit, not
pushed yet) and *Pushed*.

## How it works

- Sessions are tmux sessions named `crew_*`, each running `claude` in its own
  worktree under `~/.crew/worktrees/`.
- Quitting crew (`q`) stops its sessions and frees their branches, so you can
  check them out in any editor. Each session's folder, uncommitted changes
  included, is kept; opening the session again checks its branch back out and
  resumes the same conversation.
- Live status (working, idle, needs input) comes from Claude Code hooks that crew
  passes with `--settings`; your own settings are not changed. You get a macOS
  notification when a session needs input.
- Token usage and cost are read from Claude Code's transcripts. Cost is an
  estimate at API prices — on a Claude subscription it is not what you're billed.
- Inside crew sessions, `tab` belongs to crew, so Claude's tab autocomplete is not
  available there (`shift+tab` still works).

State lives in `~/.crew` (set `CREW_HOME` to use another directory).

## License

Apache-2.0
