package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"golang.org/x/sys/unix"
)

type sessionJSON struct {
	ID          string    `json:"id"`
	Profile     string    `json:"profile"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail,omitempty"`
	Title       string    `json:"title"`
	Cwd         string    `json:"cwd"`
	ConfigDir   string    `json:"config_dir"`
	LastPrompt  string    `json:"last_prompt,omitempty"`
	LastMessage string    `json:"last_message,omitempty"`
	TmuxPane    string    `json:"tmux_pane,omitempty"`
	PID         int       `json:"pid,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	StatusSince time.Time `json:"status_since"`
}

// recentEnded keeps finished sessions visible for a while without -a.
const recentEnded = 24 * time.Hour

func cmdList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	all := fs.Bool("a", false, "include sessions ended more than a day ago")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)

	return withStore(func(cfg *config.Config, st *store.Store) error {
		if err := sync(st); err != nil {
			return err
		}
		sessions, err := st.List()
		if err != nil {
			return err
		}
		var shown []store.Session
		for _, s := range sessions {
			if *all || s.Status != store.Ended || time.Since(s.UpdatedAt) < recentEnded {
				shown = append(shown, s)
			}
		}
		if *asJSON {
			out := make([]sessionJSON, 0, len(shown))
			for _, s := range shown {
				out = append(out, sessionJSON{s.ID, s.Profile, s.Status, s.Detail, display(s), s.Cwd,
					s.ConfigDir, s.LastPrompt, s.LastMessage, s.TmuxPane, s.PID, s.UpdatedAt, s.StatusSince})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		printGrouped(cfg, shown)
		return nil
	})
}

var statusOrder = map[string]int{
	store.Waiting: 0, store.Working: 1, store.Starting: 2, store.Idle: 3, store.Archived: 4, store.Ended: 5,
}

func printGrouped(cfg *config.Config, sessions []store.Session) {
	if len(sessions) == 0 {
		fmt.Println("no sessions yet — start one with `harness new`, or run claude with hooks installed")
		return
	}
	groups := map[string][]store.Session{}
	for _, s := range sessions {
		groups[s.Profile] = append(groups[s.Profile], s)
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)

	width := termWidth()
	for _, name := range names {
		ss := groups[name]
		sort.SliceStable(ss, func(i, j int) bool {
			oi, oj := statusOrder[ss[i].Status], statusOrder[ss[j].Status]
			if oi != oj {
				return oi < oj
			}
			return ss[i].UpdatedAt.After(ss[j].UpdatedAt)
		})
		fmt.Printf("\033[1m▾ %s\033[0m\n", name)
		for _, s := range ss {
			head := fmt.Sprintf("  %s %-8s %-9s %5s  %s", icon(s.Status), short(s.ID), statusLabel(s), ago(s.StatusSince), display(s))
			fmt.Println(truncate(head, width))
			if snip := snippet(s); snip != "" {
				fmt.Printf("\033[2m%s\033[0m\n", truncate("      "+snip, width))
			}
		}
	}
}

func icon(status string) string {
	switch status {
	case store.Working:
		return "\033[33m●\033[0m"
	case store.Waiting:
		return "\033[31m◆\033[0m"
	case store.Idle:
		return "\033[32m○\033[0m"
	case store.Starting:
		return "\033[36m◌\033[0m"
	case store.Archived:
		return "\033[2m▪\033[0m"
	default:
		return "\033[2m·\033[0m"
	}
}

func statusLabel(s store.Session) string {
	if s.Status == store.Working && s.Detail != "" {
		return truncate(s.Detail, 9)
	}
	return s.Status
}

func display(s store.Session) string {
	dir := filepath.Base(s.Cwd)
	if s.Name != "" {
		return dir + " · " + s.Name
	}
	if s.Title != "" {
		return dir + " · " + s.Title
	}
	return dir
}

// snippet is the line under a session: the pending question when waiting,
// otherwise the last assistant message (or the prompt it is working on).
func snippet(s store.Session) string {
	text := s.LastMessage
	switch {
	case s.Status == store.Waiting && s.Detail != "":
		text = "? " + s.Detail
	case s.Status == store.Working && s.LastPrompt != "":
		text = "> " + s.LastPrompt
	case text == "" && s.LastPrompt != "":
		text = "> " + s.LastPrompt
	}
	return strings.Join(strings.Fields(text), " ")
}

func short(id string) string {
	if strings.HasPrefix(id, store.PendingPrefix) {
		return id
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncate cuts to n visible runes, ignoring ANSI escape sequences.
func truncate(s string, n int) string {
	if visibleLen(s) <= n {
		return s
	}
	var b strings.Builder
	visible, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\033':
			esc = true
		case esc:
			esc = r != 'm'
		default:
			if visible == n-1 {
				b.WriteString("…\033[0m")
				return b.String()
			}
			visible++
		}
		b.WriteRune(r)
	}
	return b.String()
}

func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\033':
			esc = true
		case esc:
			esc = r != 'm'
		default:
			n++
		}
	}
	return n
}

func termWidth() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 120
	}
	return int(ws.Col)
}
