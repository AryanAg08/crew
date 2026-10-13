package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Usage is token usage and its API-price cost, summed over transcripts.
type Usage struct {
	Input, Output, CacheWrite, CacheRead int64
	Cost                                 float64
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheWrite += o.CacheWrite
	u.CacheRead += o.CacheRead
	u.Cost += o.Cost
}

func (u Usage) Tokens() int64 { return u.Input + u.Output + u.CacheWrite + u.CacheRead }

// price is $ per million tokens. Cache writes are 1.25x input (5m) or 2x (1h);
// cache reads are readX times input, which differs by model.
type price struct{ in, out, readX float64 }

// Longest matching prefix wins, so "claude-opus-5-5" beats "claude-opus-5".
var prices = map[string]price{
	"claude-fable-5-1":  {10, 50, 0.025},
	"claude-mythos-5-1": {10, 50, 0.025},
	"claude-fable-5":    {10, 50, 0.1},
	"claude-mythos-5":   {10, 50, 0.1},
	"claude-opus-5-5":   {4, 20, 0.05},
	"claude-opus-5":     {5, 25, 0.1},
	"claude-opus-4-8":   {5, 25, 0.1},
	"claude-opus-4-7":   {5, 25, 0.1},
	"claude-opus-4-6":   {5, 25, 0.1},
	"claude-opus-4-5":   {5, 25, 0.1},
	"claude-opus-4":     {15, 75, 0.1},
	"claude-sonnet-5":   {2, 10, 0.1},
	"claude-sonnet-4":   {3, 15, 0.1},
	"claude-haiku-4-5":  {1, 5, 0.1},
	"claude-3-5-haiku":  {0.8, 4, 0.1},
}

func priceFor(model string) (price, bool) {
	best, ok := "", false
	for prefix := range prices {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best, ok = prefix, true
		}
	}
	return prices[best], ok
}

type usageLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64  `json:"input_tokens"`
			Output        int64  `json:"output_tokens"`
			CacheCreation int64  `json:"cache_creation_input_tokens"`
			CacheRead     int64  `json:"cache_read_input_tokens"`
			Speed         string `json:"speed"`
			Detail        *struct {
				OneHour int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

type fileUsage struct {
	mod  time.Time
	size int64
	u    Usage
}

var usageCache = map[string]fileUsage{}

// fileUsageOf sums one transcript. A response is written as several lines
// (one per content block) that repeat the same usage, so count each id once.
func fileUsageOf(path string, fi fs.FileInfo) Usage {
	if c, ok := usageCache[path]; ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.u
	}
	f, err := os.Open(path)
	if err != nil {
		return Usage{}
	}
	defer f.Close()

	seen := map[string]bool{}
	var total Usage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"usage"`) {
			continue
		}
		var e usageLine
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.Usage == nil {
			continue
		}
		key := e.Message.ID + e.RequestID
		if key != "" && seen[key] {
			continue
		}
		seen[key] = true

		u := e.Message.Usage
		oneHour := int64(0)
		if u.Detail != nil {
			oneHour = u.Detail.OneHour
		}
		r := Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheCreation, CacheRead: u.CacheRead}
		if p, ok := priceFor(e.Message.Model); ok {
			if u.Speed == "fast" {
				p.in, p.out = p.in*2, p.out*2
			}
			fiveMin := u.CacheCreation - oneHour
			r.Cost = (float64(u.Input)*p.in +
				float64(u.Output)*p.out +
				float64(fiveMin)*p.in*1.25 +
				float64(oneHour)*p.in*2 +
				float64(u.CacheRead)*p.in*p.readX) / 1e6
		}
		total.add(r)
	}
	usageCache[path] = fileUsage{mod: fi.ModTime(), size: fi.Size(), u: total}
	return total
}

// claudeDir is where claude keeps transcripts for sessions started in dir.
func claudeDir(dir string) string {
	return filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(dir, "-"))
}

// taskUsage sums every transcript claude wrote for this task's worktree,
// including /clear restarts and subagents. The worktree is unique per task.
func taskUsage(t Task) Usage {
	var total Usage
	filepath.WalkDir(claudeDir(t.Worktree), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total.add(fileUsageOf(p, fi))
		}
		return nil
	})
	return total
}

func projectUsage(st *State, project string) Usage {
	var total Usage
	for _, t := range st.tasksOf(project) {
		total.add(taskUsage(t))
	}
	return total
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func fmtCost(c float64) string {
	if c < 0.01 && c > 0 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", c)
}
