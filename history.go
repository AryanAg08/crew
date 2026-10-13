package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type histMsg struct {
	Role string // "you" | "claude"
	Text string
}

type history struct {
	Title    string
	Msgs     []histMsg // most recent last, capped
	Modified time.Time
}

const histKeep = 40

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// transcriptPath finds claude's jsonl transcript for a session started in dir.
func transcriptPath(dir, sessionID string) string {
	p := filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(dir, "-"), sessionID+".jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// After /clear claude starts a new transcript; use the newest one in this dir.
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*.jsonl")); len(m) > 0 {
		newest, newestMod := "", time.Time{}
		for _, f := range m {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(newestMod) {
				newest, newestMod = f, fi.ModTime()
			}
		}
		return newest
	}
	if m, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

var histCache = map[string]history{}

// loadHistory reads a session's title and recent messages, cached by mtime.
func loadHistory(t Task) history {
	p := transcriptPath(t.Worktree, t.ClaudeID)
	if p == "" {
		return history{}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return history{}
	}
	if h, ok := histCache[p]; ok && h.Modified.Equal(fi.ModTime()) {
		return h
	}
	f, err := os.Open(p)
	if err != nil {
		return history{}
	}
	defer f.Close()

	h := history{Modified: fi.ModTime()}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			IsMeta      bool   `json:"isMeta"`
			AITitle     string `json:"aiTitle"`
			CustomTitle string `json:"customTitle"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "ai-title":
			if e.AITitle != "" {
				h.Title = e.AITitle
			}
		case "custom-title":
			if e.CustomTitle != "" {
				h.Title = e.CustomTitle
			}
		case "user", "assistant":
			if e.IsMeta {
				continue
			}
			text := messageText(e.Message.Content)
			if text == "" || strings.HasPrefix(text, "<") {
				continue // tool output, slash commands, system reminders
			}
			role := "you"
			if e.Type == "assistant" {
				role = "claude"
			}
			h.Msgs = append(h.Msgs, histMsg{Role: role, Text: text})
			if len(h.Msgs) > histKeep {
				h.Msgs = h.Msgs[len(h.Msgs)-histKeep:]
			}
		}
	}
	histCache[p] = h
	return h
}

// messageText returns the plain text of a message: a string, or its text blocks.
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, " ")
}

// currentBranch reads a worktree's HEAD; "" means detached (no branch chosen yet).
func currentBranch(worktree string) string {
	b, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref: refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(ref, "ref: refs/heads/")
}
