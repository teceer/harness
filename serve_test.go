package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
)

func testServer(t *testing.T) (*webServer, *http.ServeMux) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HARNESS_HOME", home)
	t.Setenv("HARNESS_TMUX_SOCKET", "harness-test-none")
	cfg, err := config.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tr := filepath.Join(home, "t.jsonl")
	os.WriteFile(tr, []byte(`{"type":"user","message":{"role":"user","content":"hello"}}`+"\n"+
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`+"\n"), 0o644)
	now := time.Now()
	st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{ID: "sess-1", Profile: "personal", Status: store.Idle, Cwd: "/tmp/p",
			TranscriptPath: tr, CreatedAt: now, UpdatedAt: now, StatusSince: now})
	})

	srv := &webServer{cfg: cfg, st: st, token: "secret-token"}
	mux := http.NewServeMux()
	srv.routes(mux)
	return srv, mux
}

func do(mux *http.ServeMux, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestWebAuth(t *testing.T) {
	_, mux := testServer(t)
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", http.StatusUnauthorized},
		{"wrong", http.StatusUnauthorized},
		{"secret-token", http.StatusOK},
	} {
		if got := do(mux, "GET", "/api/state", tc.token, "").Code; got != tc.want {
			t.Errorf("token %q: status %d, want %d", tc.token, got, tc.want)
		}
	}
	// the page itself carries no secrets and needs no token
	if got := do(mux, "GET", "/", "", "").Code; got != http.StatusOK {
		t.Errorf("page status %d", got)
	}
	if got := do(mux, "GET", "/api/state?t=secret-token", "", "").Code; got != http.StatusOK {
		t.Errorf("token in query: status %d", got)
	}
}

func TestWebStateAndMessages(t *testing.T) {
	_, mux := testServer(t)
	var st webState
	json.Unmarshal(do(mux, "GET", "/api/state", "secret-token", "").Body.Bytes(), &st)
	if len(st.Sessions) != 1 || st.Sessions[0].ID != "sess-1" || st.Sessions[0].Section != "outside harness" {
		t.Fatalf("state = %+v", st.Sessions)
	}
	if st.Sessions[0].Running {
		t.Error("a session without a harness pane must not be answerable")
	}
	if len(st.Dirs) != 1 || st.Dirs[0] != "/tmp/p" {
		t.Errorf("dirs = %v", st.Dirs)
	}

	var msgs struct {
		Messages []struct{ Role, Text string }
	}
	json.Unmarshal(do(mux, "GET", "/api/sessions/sess-1/messages", "secret-token", "").Body.Bytes(), &msgs)
	if len(msgs.Messages) != 2 || msgs.Messages[1].Text != "hi" {
		t.Errorf("messages = %+v", msgs.Messages)
	}
}

func TestWebSendGuards(t *testing.T) {
	_, mux := testServer(t)
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"unknown session", "/api/sessions/nope/send", `{"text":"x"}`, http.StatusNotFound},
		{"not in harness", "/api/sessions/sess-1/send", `{"text":"x"}`, http.StatusConflict},
		{"key not allowed", "/api/sessions/sess-1/send", `{"key":"C-c"}`, http.StatusConflict},
	}
	for _, tc := range cases {
		if got := do(mux, "POST", tc.path, "secret-token", tc.body).Code; got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := do(mux, "POST", "/api/new", "secret-token", `{"dir":"/nope/nope"}`).Code; got != http.StatusBadRequest {
		t.Errorf("new in a missing directory: status %d", got)
	}
}

func TestWebTokenPersists(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	cfg, _ := config.Load(true)
	a, err := webToken(cfg)
	if err != nil || len(a) < 16 {
		t.Fatalf("token %q err %v", a, err)
	}
	b, _ := webToken(cfg)
	if a != b {
		t.Errorf("token changed between runs: %q vs %q", a, b)
	}
	fi, err := os.Stat(filepath.Join(cfg.Home, "web-token"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v (%v)", fi.Mode().Perm(), err)
	}
}
