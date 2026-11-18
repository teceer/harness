package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/teceer/harness/internal/ansi"
	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

func TestSplitRemoteID(t *testing.T) {
	for _, tc := range []struct {
		id, host, rid string
		ok            bool
	}{
		{"box:8735e6c9", "box", "8735e6c9", true},
		{"box:pending-%0", "box", "pending-%0", true},
		{"a:b:c", "a", "b:c", true}, // one hop is ours; the rest is the remote's business
		{"8735e6c9-a7af-45d0-badb-edf34eb9b0e9", "", "", false},
		{"pending-%12", "", "", false},
		{":x", "", "", false},
		{"box:", "", "", false},
	} {
		host, rid, ok := splitRemoteID(tc.id)
		if ok != tc.ok || (ok && (host != tc.host || rid != tc.rid)) {
			t.Errorf("splitRemoteID(%q) = %q, %q, %v", tc.id, host, rid, ok)
		}
	}
	if got := shortRemote("box:8735e6c9-a7af-45d0"); got != "box:8735e6c9" {
		t.Errorf("shortRemote = %q", got)
	}
}

func TestParseAge(t *testing.T) {
	for age, want := range map[string]time.Duration{
		"56s": 56 * time.Second, "3m": 3 * time.Minute, "5h": 5 * time.Hour, "2d": 48 * time.Hour,
		"": 0, "x": 0, "-3m": 0, "12": 0,
	} {
		if got := parseAge(age); got != want {
			t.Errorf("parseAge(%q) = %v, want %v", age, got, want)
		}
	}
}

// Remote sessions form their own section and group, keep out of the ⌘[ ⌘]
// cycle and leave the local layout alone.
func TestRemoteSessionsMerge(t *testing.T) {
	now := time.Now()
	local := []store.Session{
		{ID: "l1", Profile: "personal", Status: store.Idle, TmuxPane: "%1", UpdatedAt: now},
		{ID: "l2", Profile: "work", Status: store.Working, UpdatedAt: now},
	}
	views := []remoteView{
		{Name: "box", At: now, Sessions: []webSession{
			{ID: "r1", Profile: "other", Status: store.Working, Detail: "Bash", Label: "films", Dir: "~/work", Snippet: "> go", Age: "3m"},
			{ID: "r2", Profile: "other", Status: store.Archived, Label: "old", Dir: "~/old", Age: "2h", Since: now.Add(-time.Hour).Unix()},
		}},
		{Name: "gone", Err: "timed out", Sessions: []webSession{{ID: "stale", Status: store.Idle}}},
	}
	remote := remoteSessions(views)
	if len(remote) != 2 {
		t.Fatalf("remote sessions = %+v", remote)
	}
	r1 := remote[0]
	if r1.ID != "box:r1" || r1.Profile != "box" || r1.Name != "films" || r1.Cwd != "~/work" ||
		snippet(r1) != "> go" || display(r1) != "work · films" {
		t.Errorf("r1 = %+v", r1)
	}
	if d := now.Sub(r1.StatusSince); d < 3*time.Minute || d > 3*time.Minute+time.Second {
		t.Errorf("r1 since %v ago, want 3m (from its age)", d)
	}
	if d := now.Sub(remote[1].StatusSince); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("r2 since %v ago, want 1h (since wins over age)", d)
	}
	// a label that is just the directory is not a name
	if s := remoteSession("box", webSession{ID: "x", Label: "work", Dir: "~/work"}, now); s.Name != "" || label(s) != "work" {
		t.Errorf("plain session = %+v", s)
	}

	all := append(append([]store.Session{}, local...), remote...)
	// a shown mirror pane must not turn a remote session into a local one
	all[2].TmuxPane = "%9"
	secs := sections(all, tabSessions, false, now)
	ids := func(sec section) string {
		var out []string
		for _, s := range sec.sessions {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	if len(secs) != 3 || ids(secs[0]) != "l1" || ids(secs[1]) != "l2" || secs[2].title != "remote" || ids(secs[2]) != "box:r1" {
		t.Errorf("sessions tab = %+v", secs)
	}
	if got := switchOrder(all, now); len(got) != 1 || got[0].ID != "l1" {
		t.Errorf("switch order = %+v", got)
	}
	arch := sections(all, tabArchived, false, now)
	if ids(arch[0]) != "box:r2" {
		t.Errorf("archived tab = %+v", arch)
	}
}

// fakeRemote is another machine's `harness serve`: it checks the token,
// records what it is asked and answers like serve.go does.
type fakeRemote struct {
	*httptest.Server
	mu    gosync.Mutex
	calls []string // "METHOD path?query body"
}

const remoteToken = "remote-token-0123456789"

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{}
	state := webState{Sessions: []webSession{
		{ID: "abc-1", Profile: "other", Status: store.Working, Label: "films", Dir: "~/work", Age: "3m",
			Section: "active", Number: 1, Shown: true, Running: true, Unread: true},
		{ID: "abd-2", Profile: "other", Status: store.Archived, Label: "old", Dir: "~/old", Age: "2h",
			Section: "archived", Resumable: true},
		// what the remote mirrors from its own remotes stays there
		{ID: "far:zzz", Profile: "far", Host: "far", Status: store.Idle, Section: "active"},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, state) })
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(state)
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"label": "films", "status": "working",
			"messages": []map[string]string{{"Role": "user", "Text": "hello"}, {"Role": "assistant", "Text": "hi from " + r.PathValue("id")}}})
	})
	mux.HandleFunc("GET /api/sessions/{id}/pane", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"html\":\"frame-1\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ok := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"ok": "1"}) }
	mux.HandleFunc("POST /api/sessions/{id}/send", ok)
	mux.HandleFunc("POST /api/sessions/{id}/keys", ok)
	mux.HandleFunc("POST /api/sessions/{id}/resume", ok)
	mux.HandleFunc("POST /api/sessions/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") != "1" {
			http.Error(w, "session is working", http.StatusConflict)
			return
		}
		ok(w, r)
	})
	mux.HandleFunc("POST /api/new", ok)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+remoteToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRemote) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeRemote) client(name string) *remoteClient {
	return &remoteClient{name: name, base: f.URL, token: remoteToken}
}

// A host that is away costs its timeout and nothing else: the others, and
// the local list, are unaffected.
func TestFetchRemotes(t *testing.T) {
	f := newFakeRemote(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()

	start := time.Now()
	views := fetchRemotes([]*remoteClient{
		f.client("box"),
		{name: "dead", base: dead.URL, token: "x"},
		{name: "hang", base: hang.URL, token: "x"},
		{name: "wrongtoken", base: f.URL, token: "nope"},
		{name: "bad", err: fmt.Errorf("remote bad: url must be http(s)")},
	}, 300*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v: an unreachable host must not hold the list up", d)
	}
	if v := views[0]; v.Err != "" || len(v.Sessions) != 2 || v.Sessions[0].ID != "abc-1" {
		t.Errorf("box = %+v", v)
	}
	for i, want := range map[int]string{1: "refused", 2: "timed out", 3: "unauthorized", 4: "url must be"} {
		if v := views[i]; !strings.Contains(v.Err, want) || len(v.Sessions) != 0 {
			t.Errorf("%s: err %q, want %q, sessions %d", v.Name, v.Err, want, len(v.Sessions))
		}
	}
}

func TestFindRemoteAndPreview(t *testing.T) {
	f := newFakeRemote(t)
	home := t.TempDir()
	t.Setenv("HARNESS_HOME", home)
	os.WriteFile(filepath.Join(home, "web-token"), []byte(remoteToken+"\n"), 0o600)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[remotes.box]\nurl = \""+f.URL+"\"\n"), 0o644)
	cfg, err := config.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, ws, err := findRemote(cfg, "box:abc"); err != nil || ws.ID != "abc-1" {
		t.Errorf("unique prefix: %+v, %v", ws, err)
	}
	if _, _, err := findRemote(cfg, "box:ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if _, _, err := findRemote(cfg, "box:zzz"); err == nil {
		t.Error("unknown id: want an error")
	}
	if _, _, err := findRemote(cfg, "nobox:abc"); err == nil || !strings.Contains(err.Error(), "no remote named") {
		t.Errorf("unknown remote: %v", err)
	}

	s, msgs, err := previewSession(cfg, st, "box:abc")
	if err != nil || s.ID != "box:abc-1" || s.Profile != "box" || len(msgs) != 2 || msgs[1].Text != "hi from abc-1" {
		t.Fatalf("preview = %+v, %+v, %v", s, msgs, err)
	}
	page := renderConversation(s, msgs, err, 60, 20, false, "")
	if !strings.Contains(page, "hi from abc-1") || !strings.Contains(page, "films") {
		t.Errorf("page = %q", page)
	}

	// the sidebar's actions on a remote session
	if err := remoteCall(cfg, "box:abc-1", "archive", false); err == nil || !strings.Contains(err.Error(), "session is working") {
		t.Errorf("archive of a working session: %v", err)
	}
	if err := remoteCall(cfg, "box:abc-1", "archive", true); err != nil || f.last() != "POST /api/sessions/abc-1/archive?force=1" {
		t.Errorf("forced archive: %v, %q", err, f.last())
	}
}

// primedHub is a hub that already heard from its remotes (no goroutines).
func primedHub(f *fakeRemote, extra ...*remoteClient) *remoteHub {
	h := &remoteHub{clients: append([]*remoteClient{f.client("box")}, extra...), views: map[string]remoteView{}, raw: map[string]string{}}
	h.views["box"] = f.client("box").fetch(time.Second)
	return h
}

func TestWebStateIncludesRemotes(t *testing.T) {
	f := newFakeRemote(t)
	srv, mux := testServer(t)
	srv.hub = primedHub(f, &remoteClient{name: "gone", base: "http://127.0.0.1:1", token: "x"})
	srv.hub.views["gone"] = remoteView{Name: "gone", Err: "connection refused"}

	var st webState
	json.Unmarshal(do(mux, "GET", "/api/state", "secret-token", "").Body.Bytes(), &st)
	if len(st.Sessions) != 3 || st.Sessions[0].ID != "sess-1" || st.Sessions[0].Host != "" {
		t.Fatalf("sessions = %+v", st.Sessions)
	}
	r := st.Sessions[1]
	if r.ID != "box:abc-1" || r.Host != "box" || r.Profile != "box" || r.Section != "active" || !r.Running ||
		r.Number != 0 || r.Shown || r.Since == 0 {
		t.Errorf("remote session = %+v", r)
	}
	if st.Unread != 1 {
		t.Errorf("unread = %d, want the remote's live unread session", st.Unread)
	}
	var names []string
	for _, p := range st.Profiles {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "personal,box,gone,other" {
		t.Errorf("profiles = %s", got)
	}
	want := []webRemote{{Name: "box", OK: true}, {Name: "gone", Error: "connection refused"}}
	if !reflect.DeepEqual(st.Remotes, want) {
		t.Errorf("remotes = %+v", st.Remotes)
	}

	// a remote not heard from for a while drops out instead of going stale
	v := srv.hub.views["box"]
	v.At = time.Now().Add(-2 * remoteFresh)
	srv.hub.views["box"] = v
	before := srv.hub.stamp()
	json.Unmarshal(do(mux, "GET", "/api/state", "secret-token", "").Body.Bytes(), &st)
	if len(st.Sessions) != 1 || st.Remotes[0].OK {
		t.Errorf("stale remote: sessions %d, remotes %+v", len(st.Sessions), st.Remotes)
	}
	v.At = time.Now()
	srv.hub.views["box"] = v
	if srv.hub.stamp() == before {
		t.Error("stamp must change when a remote comes back")
	}
}

func TestWebProxy(t *testing.T) {
	f := newFakeRemote(t)
	srv, mux := testServer(t)
	srv.hub = primedHub(f, &remoteClient{name: "gone", base: "http://127.0.0.1:1", token: "x"})
	calls := func() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

	// reads
	var msgs struct {
		Messages []struct{ Role, Text string }
	}
	w := do(mux, "GET", "/api/sessions/box:abc-1/messages", "secret-token", "")
	json.Unmarshal(w.Body.Bytes(), &msgs)
	if w.Code != http.StatusOK || len(msgs.Messages) != 2 || msgs.Messages[1].Text != "hi from abc-1" {
		t.Fatalf("messages: %d %s", w.Code, w.Body)
	}

	// actions: body and query pass through, our token does not
	w = do(mux, "POST", "/api/sessions/box%3Aabc-1/send?t=secret-token", "", `{"text":"go on"}`)
	if w.Code != http.StatusOK || f.last() != `POST /api/sessions/abc-1/send {"text":"go on"}` {
		t.Errorf("send: %d, remote saw %q", w.Code, f.last())
	}
	w = do(mux, "POST", "/api/sessions/box:abc-1/keys", "secret-token", `{"events":[{"k":"Enter"}]}`)
	if w.Code != http.StatusOK || f.last() != `POST /api/sessions/abc-1/keys {"events":[{"k":"Enter"}]}` {
		t.Errorf("keys: %d, remote saw %q", w.Code, f.last())
	}
	w = do(mux, "POST", "/api/sessions/box:abc-1/archive", "secret-token", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "session is working") {
		t.Errorf("archive: the remote's refusal must come through: %d %s", w.Code, w.Body)
	}
	w = do(mux, "POST", "/api/sessions/box:abc-1/archive?force=1", "secret-token", "")
	if w.Code != http.StatusOK || f.last() != "POST /api/sessions/abc-1/archive?force=1" {
		t.Errorf("forced archive: %d, remote saw %q", w.Code, f.last())
	}
	w = do(mux, "POST", "/api/new", "secret-token", `{"dir":"~/work","host":"box"}`)
	if w.Code != http.StatusOK || f.last() != `POST /api/new {"dir":"~/work"}` {
		t.Errorf("new on a remote: %d, remote saw %q", w.Code, f.last())
	}

	// guards: nothing reaches the remote without our token, an unknown or
	// absent remote is an error, local ids stay local
	n := calls()
	for _, tc := range []struct {
		name, method, path, token string
		want                      int
	}{
		{"no token", "POST", "/api/sessions/box:abc-1/send", "", http.StatusUnauthorized},
		{"wrong token", "GET", "/api/sessions/box:abc-1/messages", "wrong", http.StatusUnauthorized},
		{"unknown remote", "GET", "/api/sessions/nope:abc-1/messages", "secret-token", http.StatusNotFound},
		{"remote away", "GET", "/api/sessions/gone:abc-1/messages", "secret-token", http.StatusBadGateway},
		{"local session", "GET", "/api/sessions/sess-1/messages", "secret-token", http.StatusOK},
	} {
		if got := do(mux, tc.method, tc.path, tc.token, "").Code; got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if calls() != n {
		t.Errorf("the remote was called %d more times by requests it must not see", calls()-n)
	}
}

// Streams (the live pane, the conversation) pass through as they come.
func TestWebProxyStreams(t *testing.T) {
	f := newFakeRemote(t)
	srv, mux := testServer(t)
	srv.hub = primedHub(f)
	local := httptest.NewServer(mux)
	defer local.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", local.URL+"/api/sessions/box:abc-1/pane?t=secret-token&format=spans", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != `data: {"html":"frame-1"}` {
		t.Fatalf("first event = %q, %v", line, err)
	}
	if got := f.last(); got != "GET /api/sessions/abc-1/pane?format=spans" {
		t.Errorf("remote saw %q (the local token must not travel)", got)
	}
}

func TestRemoteHubFollows(t *testing.T) {
	f := newFakeRemote(t)
	h := &remoteHub{clients: []*remoteClient{f.client("box")}, views: map[string]remoteView{"box": {Name: "box", Err: "connecting…"}}, raw: map[string]string{}}
	if v := h.snapshot(); len(v) != 1 || v[0].Err == "" || len(v[0].Sessions) != 0 {
		t.Fatalf("before the first event: %+v", v)
	}
	before := h.stamp()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for h.stamp() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	v := h.snapshot()
	if v[0].Err != "" || len(v[0].Sessions) != 2 {
		t.Fatalf("after the first event: %+v", v)
	}
	if got := remoteSessions(v); len(got) != 2 || got[0].ID != "box:abc-1" {
		t.Errorf("sessions = %+v", got)
	}
	var none *remoteHub // no remotes configured: everything is a no-op
	if none.active() || none.stamp() != "" || none.snapshot() != nil || none.client("box") != nil {
		t.Error("nil hub must be inert")
	}
}

func TestKeyParser(t *testing.T) {
	T := func(s string) tmux.KeyEvent { return tmux.KeyEvent{Text: s} }
	K := func(s string) tmux.KeyEvent { return tmux.KeyEvent{Key: s} }
	for _, tc := range []struct {
		name  string
		reads []string
		want  []tmux.KeyEvent
		wheel int
		quit  bool
	}{
		{"text and enter", []string{"zażółć x\r"}, []tmux.KeyEvent{T("zażółć x"), K("Enter")}, 0, false},
		{"control keys", []string{"\x03\t\x7f\n\x00"}, []tmux.KeyEvent{K("C-c"), K("Tab"), K("BSpace"), K("C-j")}, 0, false},
		{"arrows", []string{"\x1b[A\x1b[B\x1bOC\x1b[D\x1b[Z"}, []tmux.KeyEvent{K("Up"), K("Down"), K("Right"), K("Left"), K("BTab")}, 0, false},
		{"navigation", []string{"\x1b[H\x1b[4~\x1b[5~\x1b[6~\x1b[3~"}, []tmux.KeyEvent{K("Home"), K("End"), K("PageUp"), K("PageDown"), K("DC")}, 0, false},
		{"alt", []string{"\x1bb\x1b\r\x1b\x7f\x1b[1;3D\x1b[1;3C"}, []tmux.KeyEvent{K("M-b"), K("M-Enter"), K("M-BSpace"), K("M-Left"), K("M-Right")}, 0, false},
		{"lone escape", []string{"\x1b"}, []tmux.KeyEvent{K("Escape")}, 0, false},
		{"escape then text", []string{"\x1b", "a"}, []tmux.KeyEvent{K("Escape"), T("a")}, 0, false},
		{"unknown sequences are dropped", []string{"a\x1b[I\x1b[O\x1b[1000~\x1b[<0;3;4Mb"}, []tmux.KeyEvent{T("ab")}, 0, false},
		{"wheel", []string{"\x1b[<64;10;5M\x1b[<64;10;5M\x1b[<65;10;5M\x1b[<64;1;1M"}, nil, -2, false},
		{"a character cut by the read", []string{"a\xc5", "\x82b"}, []tmux.KeyEvent{T("a"), T("łb")}, 0, false},
		{"a sequence cut by the read", []string{"x\x1b[", "A"}, []tmux.KeyEvent{T("x"), K("Up")}, 0, false},
		{"paste is text, line ends included", []string{"\x1b[200~one\rtwo\r\n\x1b[Athree\x1b[201~\r"},
			[]tmux.KeyEvent{T("one\ntwo\n\x1b[Athree"), K("Enter")}, 0, false},
		{"paste cut by reads", []string{"\x1b[200~ab", "cd\x1b[20", "1~e"}, []tmux.KeyEvent{T("abcde")}, 0, false},
		{"ctrl+] leaves, the rest is not sent", []string{"ab\x1dcd"}, []tmux.KeyEvent{T("ab")}, 0, true},
	} {
		var p keyParser
		var got []tmux.KeyEvent
		wheel, quit := 0, false
		for _, r := range tc.reads {
			ev, w, q := p.feed([]byte(r))
			got, wheel, quit = append(got, ev...), wheel+w, quit || q
		}
		if !reflect.DeepEqual(got, tc.want) || wheel != tc.wheel || quit != tc.quit {
			t.Errorf("%s: got %+v wheel %d quit %v", tc.name, got, wheel, quit)
		}
		for _, ev := range got {
			if ev.Key != "" && !liveKeyRe.MatchString(ev.Key) {
				t.Errorf("%s: key %q is not one the remote accepts", tc.name, ev.Key)
			}
		}
	}
	merged := mergeText([]tmux.KeyEvent{T("a"), T("b"), K("Enter"), T("c")})
	if !reflect.DeepEqual(merged, []tmux.KeyEvent{T("ab"), K("Enter"), T("c")}) {
		t.Errorf("mergeText = %+v", merged)
	}
}

func TestRenderMirror(t *testing.T) {
	lines := [][]ansi.Seg{
		{{Text: "top"}},
		{{Text: "red", FG: "#ff0000", Bold: true}, {Text: " plain"}},
		{{Text: "a very long line that does not fit"}},
		{{Text: "❯ prompt", BG: "#000080"}},
	}
	out := renderMirror(lines, "box:abc", "", 12, 4) // 3 rows of pane under the title
	rows := strings.Split(out, "\r\n")
	if len(rows) != 4 {
		t.Fatalf("rows = %d: %q", len(rows), out)
	}
	if !strings.HasPrefix(rows[0], "\x1b[H") || !strings.Contains(rows[0], "box:abc") {
		t.Errorf("title row = %q", rows[0])
	}
	// the bottom of a taller pane is what shows; "top" is cut
	if strings.Contains(out, "top") || !strings.Contains(rows[1], "\x1b[0;1;38;2;255;0;0mred") ||
		!strings.Contains(rows[2], "a very long ") || strings.Contains(rows[2], "line that") ||
		!strings.Contains(rows[3], "\x1b[0;48;2;0;0;128m❯ prompt") {
		t.Errorf("out = %q", out)
	}
	for _, r := range rows {
		if !strings.HasSuffix(r, "\x1b[K\x1b[0m") && !strings.HasSuffix(r, "\x1b[0m\x1b[K") {
			t.Errorf("row not cleared to the end: %q", r)
		}
	}
	// a shorter pane is top aligned and the rest blanked; trouble shows in the title
	out = renderMirror(lines[:1], "box:abc", "timed out — retrying", 40, 5)
	if rows := strings.Split(out, "\r\n"); len(rows) != 5 || !strings.Contains(rows[0], "retrying") ||
		!strings.Contains(rows[1], "top") || rows[4] != "\x1b[0m\x1b[K" {
		t.Errorf("short pane = %q", out)
	}
}
