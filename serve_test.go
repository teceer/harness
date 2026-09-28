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
	if n := len(st.Profiles); n == 0 || st.Profiles[n-1].Name != "other" || st.Profiles[n-1].Roots == nil {
		t.Errorf("profiles = %+v", st.Profiles)
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
	if got := do(mux, "GET", "/api/sessions/sess-1/scrollback", "secret-token", "").Code; got != http.StatusConflict {
		t.Errorf("scrollback outside harness: status %d", got)
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

func TestPushRegisterAndSend(t *testing.T) {
	srv, mux := testServer(t)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"token":"not-a-token"}`, http.StatusBadRequest},
		{`{"token":"ExponentPushToken[gone]"}`, http.StatusOK},
		{`{"token":"ExponentPushToken[ok]","device":"iPhone"}`, http.StatusOK},
	} {
		if w := do(mux, "POST", "/api/push/register", "secret-token", tc.body); w.Code != tc.want {
			t.Fatalf("register %s: %d, want %d", tc.body, w.Code, tc.want)
		}
	}
	if w := do(mux, "POST", "/api/push/register", "", `{"token":"ExponentPushToken[x]"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("register without token: %d", w.Code)
	}

	var got []expoMessage
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		var data []map[string]any
		for _, m := range got {
			if m.To == "ExponentPushToken[gone]" {
				data = append(data, map[string]any{"status": "error", "message": "gone",
					"details": map[string]string{"error": "DeviceNotRegistered"}})
			} else {
				data = append(data, map[string]any{"status": "ok", "id": "x"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer fake.Close()
	old := expoPushURL
	expoPushURL = fake.URL
	defer func() { expoPushURL = old }()

	se := store.Session{ID: "sess-1", Status: store.Waiting, TmuxPane: "%1"}
	srv.sendPush(se, "⏸ p is waiting", "Allow Bash?")
	if len(got) != 2 {
		t.Fatalf("sent %d messages, want 2", len(got))
	}
	for _, m := range got {
		if m.CategoryID != "waiting" || m.Data["sessionId"] != "sess-1" || m.Body != "Allow Bash?" {
			t.Fatalf("message %+v", m)
		}
	}
	if left := srv.pushStore().list(); len(left) != 1 || left[0] != "ExponentPushToken[ok]" {
		t.Fatalf("tokens after DeviceNotRegistered: %v", left)
	}

	// A finished turn has nothing to answer: no action buttons.
	srv.sendPush(store.Session{ID: "sess-1", Status: store.Idle}, "✅ done", "")
	if len(got) != 1 || got[0].CategoryID != "" {
		t.Fatalf("idle message %+v", got)
	}

	if w := do(mux, "POST", "/api/push/unregister", "secret-token", `{"token":"ExponentPushToken[ok]"}`); w.Code != http.StatusOK {
		t.Fatalf("unregister: %d", w.Code)
	}
	if left := srv.pushStore().list(); len(left) != 0 {
		t.Fatalf("tokens after unregister: %v", left)
	}
}

func TestUnread(t *testing.T) {
	srv, mux := testServer(t)
	state := func() webState {
		var st webState
		json.Unmarshal(do(mux, "GET", "/api/state", "secret-token", "").Body.Bytes(), &st)
		return st
	}
	// Finished before tracking started: read.
	if st := state(); st.Sessions[0].Unread {
		t.Fatal("an old finish must not count as unread")
	}

	// A turn finishes after that: unread until the session is opened.
	later := time.Now().Add(2 * time.Second)
	se, _ := srv.st.Find("sess-1")
	se.Status, se.StatusSince = store.Idle, later
	srv.st.Tx(func(tx *store.Tx) error { return tx.Put(se) })
	if st := state(); !st.Sessions[0].Unread {
		t.Fatalf("a new finish must be unread: %+v", st.Sessions[0])
	}
	srv.readMu.Lock()
	rs := srv.loadRead()
	rs.Seen["sess-1"] = later.Add(time.Second) // opened after it finished
	srv.saveRead(rs)
	srv.readMu.Unlock()
	if st := state(); st.Sessions[0].Unread {
		t.Fatal("opened: must be read")
	}

	// Marked by hand, then opened again.
	if w := do(mux, "POST", "/api/sessions/sess-1/unread", "secret-token", ""); w.Code != http.StatusOK {
		t.Fatalf("unread: %d", w.Code)
	}
	if st := state(); !st.Sessions[0].Unread {
		t.Fatal("marked unread must show as unread")
	}
	if w := do(mux, "POST", "/api/sessions/sess-1/read", "secret-token", ""); w.Code != http.StatusOK {
		t.Fatalf("read: %d", w.Code)
	}
	if st := state(); st.Sessions[0].Unread {
		t.Fatal("read must clear the mark")
	}
}
