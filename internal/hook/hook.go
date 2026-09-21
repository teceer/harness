// Package hook turns Claude Code hook events into session state.
//
// Contract with Claude Code: never block, never fail, never print. Stdout of
// SessionStart/UserPromptSubmit hooks is injected into the model context,
// and a non-zero exit shows up as an error, so the caller always exits 0.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/proc"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
	"github.com/teceer/harness/internal/transcript"
)

// Input is the subset of the hook payload we use; unknown fields are ignored.
type Input struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	Cwd                  string `json:"cwd"`
	Event                string `json:"hook_event_name"`
	Source               string `json:"source"` // SessionStart: startup|resume|clear|compact
	Reason               string `json:"reason"` // SessionEnd
	Prompt               string `json:"prompt"`
	ToolName             string `json:"tool_name"`
	NotificationType     string `json:"notification_type"`
	Message              string `json:"message"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// Env is what the hook learns from its process environment.
type Env struct {
	TmuxPane  string
	ClaudeDir string
	PID       int
}

func EnvFromOS() Env {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = config.DefaultClaudeDir()
	}
	env := Env{ClaudeDir: filepath.Clean(config.Expand(dir))}
	// Tests feed hooks from a shell that may itself run under claude; the
	// ancestor walk would then pick that process. -1 disables the lookup.
	if v := os.Getenv("HARNESS_HOOK_PID"); v != "" {
		env.PID, _ = strconv.Atoi(v)
	}
	env.TmuxPane = paneFromEnv(os.Getenv)
	return env
}

// paneFromEnv finds the harness tmux pane the session runs in. Harness
// starts claude with TMUX hidden (for truecolor) and HARNESS_PANE set; a
// claude started by hand inside the harness server still has TMUX. Pane ids
// are per server, so panes of any other tmux are ignored.
func paneFromEnv(getenv func(string) string) string {
	if p := getenv("HARNESS_PANE"); p != "" && getenv("HARNESS_SOCKET") == tmux.Socket {
		return p
	}
	if tmux.OwnsTMUXEnv(getenv("TMUX")) {
		return getenv("TMUX_PANE")
	}
	return ""
}

const (
	maxPrompt  = 500
	maxMessage = 2000
)

// Run reads one event from r and records it.
func Run(r io.Reader, cfg *config.Config, st *store.Store, env Env) error {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return err
	}
	if os.Getenv("HARNESS_DEBUG") != "" {
		debugDump(cfg, data)
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if in.SessionID == "" {
		return fmt.Errorf("payload without session_id (event %q)", in.Event)
	}
	// The pid is looked up once per session (SessionStart, or the first event
	// of a session that was already running when hooks were installed).
	if env.PID < 0 {
		env.PID = 0
	} else if env.PID == 0 && (in.Event == "SessionStart" || st.PID(in.SessionID) == 0) && in.Event != "SessionEnd" {
		env.PID = proc.ClaudeAncestor()
	}
	return st.Tx(func(tx *store.Tx) error { return apply(tx, cfg, in, env, time.Now()) })
}

func apply(tx *store.Tx, cfg *config.Config, in Input, env Env, now time.Time) error {
	s, exists, err := tx.Get(in.SessionID)
	if err != nil {
		return err
	}

	if in.Event == "SessionStart" && env.TmuxPane != "" {
		// Whatever else lived in this pane is gone: either our own placeholder
		// (claim it, keeping name/profile) or a session replaced by /clear.
		others, err := tx.ByPane(env.TmuxPane)
		if err != nil {
			return err
		}
		for _, o := range others {
			if o.ID == in.SessionID {
				continue
			}
			if strings.HasPrefix(o.ID, store.PendingPrefix) || (o.Status == store.Starting && !exists) {
				if err := tx.Delete(o.ID); err != nil {
					return err
				}
				if !exists {
					s, exists = o, true
					s.ID = in.SessionID
				}
				continue
			}
			if o.Live() {
				o.SetStatus(store.Ended, now)
				o.Detail = "replaced in pane"
			}
			o.TmuxPane, o.PID = "", 0
			if err := tx.Put(o); err != nil {
				return err
			}
		}
	}

	if in.Event == "SessionEnd" {
		if !exists {
			return nil // e.g. an empty session harness archived and dropped
		}
		// The row now belongs to another process (moved into or resumed in a
		// harness pane): the old process saying goodbye must not end it.
		if s.TmuxPane != "" && s.TmuxPane != env.TmuxPane {
			return nil
		}
	}
	if !exists {
		prof, _ := cfg.ProfileFor(in.Cwd, env.ClaudeDir)
		s = store.Session{
			ID: in.SessionID, Profile: prof.Name, Cwd: in.Cwd,
			Status: store.Idle, CreatedAt: now, StatusSince: now,
		}
	}
	if s.ConfigDir == "" {
		s.ConfigDir = env.ClaudeDir
	}
	if in.Cwd != "" && s.Cwd == "" {
		s.Cwd = in.Cwd
	}
	if in.TranscriptPath != "" {
		s.TranscriptPath = in.TranscriptPath
	}
	if env.TmuxPane != "" {
		s.TmuxPane = env.TmuxPane
	}
	if env.PID != 0 {
		s.PID = env.PID
	}
	s.UpdatedAt = now

	// A session that runs again is no longer archived; a late SessionEnd
	// from the process harness killed must not un-archive it.
	if in.Event != "SessionEnd" {
		s.ArchivedAt = time.Time{}
	}

	switch in.Event {
	case "SessionStart":
		s.SetStatus(store.Idle, now)
		s.Detail = in.Source
		if in.Source == "clear" {
			s.LastMessage, s.LastPrompt = "", ""
		}
		fillFromTranscript(&s, in)
	case "UserPromptSubmit":
		s.SetStatus(store.Working, now)
		s.Detail = ""
		s.LastPrompt = clip(in.Prompt, maxPrompt)
	case "PreToolUse":
		s.SetStatus(store.Working, now)
		s.Detail = in.ToolName
	case "PostToolUse":
		s.SetStatus(store.Working, now)
		s.Detail = ""
	case "Notification":
		switch in.NotificationType {
		case "idle_prompt":
			s.SetStatus(store.Idle, now)
		case "auth_success":
		default: // permission_prompt, elicitation_dialog, …
			s.SetStatus(store.Waiting, now)
			s.Detail = clip(in.Message, 200)
		}
	case "Stop":
		s.SetStatus(store.Idle, now)
		s.Detail = ""
		if in.LastAssistantMessage != "" {
			s.LastMessage = clip(in.LastAssistantMessage, maxMessage)
		}
		fillFromTranscript(&s, in)
	case "SessionEnd":
		if s.Status != store.Archived {
			s.SetStatus(store.Ended, now)
			s.Detail = in.Reason
		}
		s.PID, s.TmuxPane = 0, ""
	}
	return tx.Put(s)
}

// fillFromTranscript refreshes the title and, for Stop without a payload
// message, the last assistant text.
func fillFromTranscript(s *store.Session, in Input) {
	if s.TranscriptPath == "" {
		return
	}
	info, err := transcript.Read(s.TranscriptPath)
	if err != nil {
		return
	}
	if info.Title != "" {
		s.Title = info.Title
	}
	if info.LastMessage != "" && (in.Event != "Stop" || in.LastAssistantMessage == "") {
		s.LastMessage = clip(info.LastMessage, maxMessage)
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func debugDump(cfg *config.Config, data []byte) {
	f, err := os.OpenFile(filepath.Join(cfg.Home, "hook-debug.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}
