package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
)

const testConfig = `
[profiles.personal]
roots = ["~/code"]

[profiles.work]
roots = ["~/work"]
env = { CLAUDE_CONFIG_DIR = "~/.claude-work" }
`

func setup(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HARNESS_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o644)
	cfg, err := config.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st
}

func send(t *testing.T, cfg *config.Config, st *store.Store, env Env, payload string) {
	t.Helper()
	if env.PID == 0 {
		env.PID = 4242 // skip the ps walk in tests
	}
	if err := Run(strings.NewReader(payload), cfg, st, env); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, st *store.Store, id string) store.Session {
	t.Helper()
	s, err := st.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLifecycle(t *testing.T) {
	cfg, st := setup(t)
	home, _ := os.UserHomeDir()
	workDir := filepath.Join(home, ".claude-work")
	env := Env{TmuxPane: "%7", ClaudeDir: workDir}

	tr := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(tr, []byte(`{"type":"ai-title","aiTitle":"Fix login"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done, tests pass."}]}}
`), 0o644)

	cwd := filepath.Join(home, "work", "api")
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"SessionStart","source":"startup","cwd":"`+cwd+`","transcript_path":"`+tr+`"}`)
	s := get(t, st, "abc")
	if s.Profile != "work" || s.Status != store.Idle || s.TmuxPane != "%7" || s.ConfigDir != workDir || s.Title != "Fix login" {
		t.Fatalf("after start: %+v", s)
	}

	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"UserPromptSubmit","prompt":"fix login"}`)
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"PreToolUse","tool_name":"Bash"}`)
	if s = get(t, st, "abc"); s.Status != store.Working || s.Detail != "Bash" || s.LastPrompt != "fix login" {
		t.Fatalf("working: %+v", s)
	}
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`)
	if s = get(t, st, "abc"); s.Status != store.Waiting {
		t.Fatalf("waiting: %+v", s)
	}
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"Stop"}`)
	if s = get(t, st, "abc"); s.Status != store.Idle || s.LastMessage != "Done, tests pass." {
		t.Fatalf("stop: %+v", s)
	}
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"Stop","last_assistant_message":"from payload"}`)
	if s = get(t, st, "abc"); s.LastMessage != "from payload" {
		t.Fatalf("payload message should win: %q", s.LastMessage)
	}
	send(t, cfg, st, env, `{"session_id":"abc-1","hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`)
	if s = get(t, st, "abc"); s.Status != store.Ended || s.TmuxPane != "" || s.PID != 0 {
		t.Fatalf("end: %+v", s)
	}
}

func TestPlaceholderClaimedAndClearReplaces(t *testing.T) {
	cfg, st := setup(t)
	now := time.Now()
	st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{ID: store.PendingPrefix + "%3", Profile: "personal", Name: "my name",
			Cwd: "/tmp/x", Status: store.Starting, TmuxPane: "%3", CreatedAt: now, UpdatedAt: now, StatusSince: now})
	})
	env := Env{TmuxPane: "%3", ClaudeDir: "/c"}
	send(t, cfg, st, env, `{"session_id":"s1","hook_event_name":"SessionStart","source":"startup","cwd":"/tmp/x"}`)
	s := get(t, st, "s1")
	if s.Profile != "personal" || s.Name != "my name" || s.Status != store.Idle {
		t.Fatalf("placeholder not claimed: %+v", s)
	}
	if _, err := st.Find(store.PendingPrefix + "%3"); err == nil {
		t.Fatal("placeholder still present")
	}

	send(t, cfg, st, env, `{"session_id":"s2","hook_event_name":"SessionStart","source":"clear","cwd":"/tmp/x"}`)
	if old := get(t, st, "s1"); old.Status != store.Ended || old.TmuxPane != "" {
		t.Fatalf("/clear should end the old session: %+v", old)
	}
	if s2 := get(t, st, "s2"); s2.TmuxPane != "%3" || s2.Status != store.Idle {
		t.Fatalf("new session: %+v", s2)
	}
}

func TestResumeWithNewIDClaimsStartingRow(t *testing.T) {
	cfg, st := setup(t)
	now := time.Now()
	st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{ID: "old", Profile: "work", Title: "T", Cwd: "/w", Status: store.Starting,
			TmuxPane: "%9", CreatedAt: now, UpdatedAt: now, StatusSince: now})
	})
	send(t, cfg, st, Env{TmuxPane: "%9", ClaudeDir: "/c"}, `{"session_id":"new","hook_event_name":"SessionStart","source":"resume","cwd":"/w"}`)
	if s := get(t, st, "new"); s.Profile != "work" || s.Status != store.Idle {
		t.Fatalf("resume row: %+v", s)
	}
}

func TestArchivedSurvivesSessionEnd(t *testing.T) {
	cfg, st := setup(t)
	now := time.Now()
	st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{ID: "a", Status: store.Archived, ArchivedAt: now, TmuxPane: "%1",
			CreatedAt: now, UpdatedAt: now, StatusSince: now})
	})
	send(t, cfg, st, Env{TmuxPane: "%1", ClaudeDir: "/c"}, `{"session_id":"a","hook_event_name":"SessionEnd","reason":"other"}`)
	if s := get(t, st, "a"); s.Status != store.Archived || s.ArchivedAt.IsZero() {
		t.Fatalf("archive lost: %+v", s)
	}
}

func TestSessionEndOfUnknownSessionIsIgnored(t *testing.T) {
	cfg, st := setup(t)
	send(t, cfg, st, Env{ClaudeDir: "/c"}, `{"session_id":"gone","hook_event_name":"SessionEnd","reason":"other"}`)
	if _, err := st.Find("gone"); err == nil {
		t.Fatal("SessionEnd must not create a row")
	}
}

func TestSessionEndFromOldProcessIgnoredAfterMove(t *testing.T) {
	cfg, st := setup(t)
	now := time.Now()
	st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{ID: "m", Status: store.Starting, TmuxPane: "%4",
			CreatedAt: now, UpdatedAt: now, StatusSince: now})
	})
	// SessionEnd of the terminal-tab process (no harness pane) arrives late.
	send(t, cfg, st, Env{ClaudeDir: "/c"}, `{"session_id":"m","hook_event_name":"SessionEnd","reason":"other"}`)
	if s := get(t, st, "m"); s.Status != store.Starting || s.TmuxPane != "%4" {
		t.Fatalf("moved session clobbered: %+v", s)
	}
}

func TestPaneFromEnv(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HARNESS_PANE": "%3", "HARNESS_SOCKET": "harness"}, "%3"},
		{map[string]string{"HARNESS_PANE": "%3", "HARNESS_SOCKET": "other"}, ""},
		{map[string]string{"TMUX": "/private/tmp/tmux-501/harness,123,0", "TMUX_PANE": "%5"}, "%5"},
		{map[string]string{"TMUX": "/private/tmp/tmux-501/default,123,0", "TMUX_PANE": "%5"}, ""},
		{map[string]string{}, ""},
	}
	for _, tc := range cases {
		if got := paneFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("paneFromEnv(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}
