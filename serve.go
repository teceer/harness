package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
	"github.com/teceer/harness/internal/transcript"
)

//go:embed web/index.html
var webFS embed.FS

// cmdServe runs the remote control: the same session list as the sidebar,
// readable and answerable from a phone. It listens on the Tailscale address
// (and loopback) only — never on a public interface — and every API call
// carries the token from ~/.harness/web-token.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "extra address to listen on (default: the Tailscale one)")
	port := fs.Int("port", 7777, "port")
	public := fs.Bool("i-know-this-exposes-my-mac", false, "also listen on every interface")
	fs.Parse(args)

	return withStore(func(cfg *config.Config, st *store.Store) error {
		token, err := webToken(cfg)
		if err != nil {
			return err
		}
		srv := &webServer{cfg: cfg, st: st, token: token}
		mux := http.NewServeMux()
		srv.routes(mux)

		hosts := []string{"127.0.0.1"}
		switch {
		case *public:
			hosts = []string{""}
		case *addr != "":
			hosts = append(hosts, *addr)
		default:
			if ip := tailscaleIP(); ip != "" {
				hosts = append(hosts, ip)
			} else {
				fmt.Fprintln(os.Stderr, "harness: no Tailscale address found — serving on 127.0.0.1 only")
			}
		}

		go srv.watch()
		errs := make(chan error, len(hosts))
		for _, h := range hosts {
			ln, err := net.Listen("tcp", net.JoinHostPort(h, fmt.Sprint(*port)))
			if err != nil {
				return err
			}
			fmt.Printf("harness: http://%s/?t=%s\n", ln.Addr(), token)
			go func() { errs <- http.Serve(ln, mux) }()
		}
		return <-errs
	})
}

type webServer struct {
	cfg   *config.Config
	st    *store.Store
	token string
}

func (s *webServer) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.page)
	mux.Handle("GET /api/state", s.auth(s.state))
	mux.Handle("GET /api/events", s.auth(s.events))
	mux.Handle("GET /api/sessions/{id}/messages", s.auth(s.messages))
	mux.Handle("POST /api/sessions/{id}/send", s.auth(s.send))
	mux.Handle("POST /api/sessions/{id}/archive", s.auth(s.archiveSession))
	mux.Handle("POST /api/sessions/{id}/resume", s.auth(s.resume))
	mux.Handle("POST /api/new", s.auth(s.newSession))
}

// auth: the tailnet already limits who can reach us; the token is the
// second lock, so a stray tab or another device on the tailnet cannot
// drive Claude sessions.
func (s *webServer) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if given == "" {
			given = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func (s *webServer) page(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

// ---- state ----

type webSession struct {
	ID        string `json:"id"`
	Profile   string `json:"profile"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Label     string `json:"label"`
	Dir       string `json:"dir"`
	Snippet   string `json:"snippet,omitempty"`
	Age       string `json:"age"`
	Section   string `json:"section"`
	Number    int    `json:"number,omitempty"`
	Shown     bool   `json:"shown"`
	Running   bool   `json:"running"` // has a live harness pane to type into
	Resumable bool   `json:"resumable"`
}

type webState struct {
	Sessions []webSession `json:"sessions"`
	Dirs     []string     `json:"dirs"` // known project directories, for a new session
}

func (s *webServer) snapshot() (webState, error) {
	if err := sync(s.st); err != nil {
		return webState{}, err
	}
	all, err := s.st.List()
	if err != nil {
		return webState{}, err
	}
	now := time.Now()
	numbers := map[string]int{}
	for i, x := range switchOrder(all, now) {
		if i < 9 {
			numbers[x.ID] = i + 1
		}
	}
	shown := ""
	if sb := tmux.Sidebar(); sb != "" {
		shown = tmux.Shown(sb)
	}
	panes := tmux.Panes()

	var out webState
	seen := map[string]bool{}
	for _, t := range []tab{tabSessions, tabArchived} {
		for _, sec := range sections(all, t, true, now) {
			for _, x := range sec.sessions {
				_, live := panes[x.TmuxPane]
				out.Sessions = append(out.Sessions, webSession{
					ID: x.ID, Profile: x.Profile, Status: x.Status, Detail: x.Detail,
					Label: label(x), Dir: collapseHome(x.Cwd), Snippet: snippet(x),
					Age: ago(x.StatusSince), Section: sec.title, Number: numbers[x.ID],
					Shown: x.TmuxPane != "" && x.TmuxPane == shown, Running: live,
					Resumable: !x.Live() && hasTranscript(x),
				})
			}
		}
		for _, x := range all { // directories of every session, newest first
			if d := collapseHome(x.Cwd); x.Cwd != "" && !seen[d] {
				seen[d] = true
				out.Dirs = append(out.Dirs, d)
			}
		}
	}
	return out, nil
}

func (s *webServer) state(w http.ResponseWriter, r *http.Request) {
	st, err := s.snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

// events streams the state whenever it changes, so a phone shows the same
// statuses as the sidebar without polling.
func (s *webServer) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	last, lastSend := "", time.Time{}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		stamp := stateStamp(s.cfg.Home)
		if stamp == last && time.Since(lastSend) < 5*time.Second {
			continue
		}
		last, lastSend = stamp, time.Now()
		st, err := s.snapshot()
		if err != nil {
			continue
		}
		data, _ := json.Marshal(st)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
}

func (s *webServer) messages(w http.ResponseWriter, r *http.Request) {
	se, err := s.st.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	msgs, err := transcript.Tail(se.TranscriptPath, 30)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"label": label(se), "status": se.Status, "detail": se.Detail, "messages": msgs})
}

// ---- actions ----

type sendBody struct {
	Text string `json:"text"` // typed into the session, then Enter
	Key  string `json:"key"`  // a single key instead: enter, escape, 1, 2, y…
}

func (s *webServer) send(w http.ResponseWriter, r *http.Request) {
	var body sendBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	se, err := s.st.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if se.TmuxPane == "" || !paneExists(se.TmuxPane) {
		http.Error(w, "session is not running in harness", http.StatusConflict)
		return
	}
	if body.Key != "" {
		if !allowedKeys[body.Key] {
			http.Error(w, "key not allowed", http.StatusBadRequest)
			return
		}
		err = tmux.SendKey(se.TmuxPane, body.Key)
	} else if strings.TrimSpace(body.Text) != "" {
		err = tmux.SendText(se.TmuxPane, body.Text)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logAction(r, "send %s to %s", cmp(body.Key, "text"), short(se.ID))
	writeJSON(w, map[string]string{"ok": "1"})
}

// allowedKeys keeps the remote to answering prompts: no arbitrary key
// sequences, and nothing that could drive a shell.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "1": true, "2": true, "3": true,
	"y": true, "n": true, "Up": true, "Down": true, "Tab": true,
}

func (s *webServer) archiveSession(w http.ResponseWriter, r *http.Request) {
	se, err := s.st.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := archive(s.st, se, r.URL.Query().Get("force") == "1"); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.logAction(r, "archive %s", short(se.ID))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *webServer) resume(w http.ResponseWriter, r *http.Request) {
	se, err := s.st.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := sync(s.st); err == nil {
		se, _ = s.st.Find(se.ID)
	}
	pane, err := resumeSession(s.cfg, s.st, se)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	tmux.FitToSlot(pane, s.cfg.SidebarWidth)
	s.logAction(r, "resume %s", short(se.ID))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *webServer) newSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir := resolveDir(body.Dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		http.Error(w, "not a directory: "+body.Dir, http.StatusBadRequest)
		return
	}
	pane, prof, err := newSession(s.cfg, s.st, dir, "", "", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmux.FitToSlot(pane, s.cfg.SidebarWidth)
	s.logAction(r, "new session in %s [%s]", dir, prof.Name)
	writeJSON(w, map[string]string{"ok": "1"})
}

// ---- notifications ----

// watch turns status changes into notifications: a session waiting for an
// answer, or one that just finished its turn.
func (s *webServer) watch() {
	if s.cfg.NotifyCommand == "" {
		return
	}
	was := map[string]string{}
	first := true
	for ; ; time.Sleep(time.Second) {
		all, err := s.st.List()
		if err != nil {
			continue
		}
		for _, x := range all {
			prev, known := was[x.ID], was[x.ID] != ""
			was[x.ID] = x.Status
			if first || !known || prev == x.Status {
				continue
			}
			switch {
			case x.Status == store.Waiting:
				s.notify(x, "⏸ "+label(x)+" is waiting", x.Detail)
			case x.Status == store.Idle && prev == store.Working:
				s.notify(x, "✅ "+label(x)+" finished", firstLine(x.LastMessage))
			}
		}
		first = false
	}
}

func (s *webServer) notify(se store.Session, title, body string) {
	msg := title
	if body != "" {
		msg += "\n" + trim(body, 300)
	}
	if s.cfg.WebURL != "" {
		msg += "\n" + strings.TrimRight(s.cfg.WebURL, "/") + "/?t=" + url.QueryEscape(s.token) +
			"#" + url.PathEscape(se.ID)
	}
	cmd := exec.Command("sh", "-c", s.cfg.NotifyCommand)
	cmd.Env = append(os.Environ(), "HARNESS_MESSAGE="+msg, "HARNESS_SESSION="+se.ID, "HARNESS_STATUS="+se.Status)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("notify: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *webServer) logAction(r *http.Request, format string, a ...any) {
	log.Printf("%s: %s", r.RemoteAddr, fmt.Sprintf(format, a...))
}

// trim shortens a notification body to n runes.
func trim(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func cmp(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

// webToken reads (or creates) the shared secret every API call carries.
func webToken(cfg *config.Config) (string, error) {
	path := filepath.Join(cfg.Home, "web-token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// tailscaleIP is this machine's tailnet address, "" when Tailscale is not
// running. The CLI ships inside the app bundle on macOS.
func tailscaleIP() string {
	for _, bin := range []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"} {
		out, err := exec.Command(bin, "ip", "-4").Output()
		if err == nil {
			if ip := strings.TrimSpace(strings.Split(string(out), "\n")[0]); ip != "" {
				return ip
			}
		}
	}
	return ""
}
