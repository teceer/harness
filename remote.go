package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
	"github.com/teceer/harness/internal/transcript"
)

// Remote hosts: other machines running `harness serve` ([remotes.<name>] in
// config.toml). Their sessions are listed next to the local ones under the
// id "<name>:<session id>", and everything done to them goes through that
// machine's API — no SSH, no shared state.

// splitRemoteID splits "<remote>:<id>". Local ids (UUIDs, "pending-…")
// never contain a colon.
func splitRemoteID(id string) (host, rid string, ok bool) {
	host, rid, ok = strings.Cut(id, ":")
	return host, rid, ok && host != "" && rid != ""
}

func isRemoteID(id string) bool { _, _, ok := splitRemoteID(id); return ok }

// shortRemote is short() for a remote id: the prefix kept, the id cut.
func shortRemote(id string) string {
	if host, rid, ok := splitRemoteID(id); ok {
		return host + ":" + short(rid)
	}
	return short(id)
}

// remoteQuick bounds a one-shot call (ls, preview, an action): an
// unreachable host costs this much and no more.
const remoteQuick = 3 * time.Second

// remoteTransport is shared by every remote call. Short dial and handshake
// limits keep a dead host from hanging anything; keep-alives notice a
// connection that silently went away under a long-lived stream.
var remoteTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:         5 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3},
	}).DialContext,
	TLSHandshakeTimeout:   5 * time.Second,
	ResponseHeaderTimeout: 15 * time.Second,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
	ForceAttemptHTTP2:     true,
}

var remoteHTTP = &http.Client{Transport: remoteTransport}

// remoteClient talks to one remote's API.
type remoteClient struct {
	name  string
	base  string // no trailing slash
	token string
	err   error // a configuration problem: every call fails with it
}

// remoteClients builds a client per configured remote, in display order.
func remoteClients(cfg *config.Config) []*remoteClient {
	var out []*remoteClient
	for _, name := range cfg.RemoteNames() {
		r := cfg.Remotes[name]
		c := &remoteClient{name: name, base: r.URL, err: r.Validate()}
		if c.err == nil {
			c.token, c.err = cfg.RemoteToken(r)
		}
		out = append(out, c)
	}
	return out
}

func remoteClientFor(cfg *config.Config, host string) (*remoteClient, error) {
	for _, c := range remoteClients(cfg) {
		if c.name == host {
			return c, c.err
		}
	}
	return nil, fmt.Errorf("no remote named %q in config.toml", host)
}

func (c *remoteClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if c.err != nil {
		return nil, c.err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := remoteHTTP.Do(req)
	if err != nil {
		return nil, cleanNetErr(err)
	}
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		return nil, &remoteStatusError{res.StatusCode, strings.TrimSpace(string(msg))}
	}
	return res, nil
}

// remoteStatusError is an answer from the remote other than 200.
type remoteStatusError struct {
	code int
	msg  string
}

func (e *remoteStatusError) Error() string {
	if e.msg == "" {
		return http.StatusText(e.code)
	}
	return e.msg
}

// cleanNetErr drops the URL from a transport error: it is noise in a
// one-line message, and nothing about a request belongs in logs.
func cleanNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("timed out")
		}
		return ue.Err
	}
	return err
}

// call does one JSON request within remoteQuick and decodes the answer
// into out (nil: ignored).
func (c *remoteClient) call(method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteQuick)
	defer cancel()
	res, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func sessionPath(rid, what string) string {
	return "/api/sessions/" + url.PathEscape(rid) + "/" + what
}

// sse reads a server-sent event stream until it ends or ctx is done,
// handing each data payload to onData.
func (c *remoteClient) sse(ctx context.Context, path string, onData func([]byte)) error {
	res, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	rd := bufio.NewReaderSize(res.Body, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			onData(bytes.TrimRight(data, "\r\n"))
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return cleanNetErr(err)
		}
	}
}

// ---- remote sessions as local ones ----

// remoteView is what is known about one remote right now.
type remoteView struct {
	Name     string
	Sessions []webSession // as the remote reports them, ids unprefixed
	At       time.Time    // when Sessions were received
	Err      string       // why it cannot be shown; "" when reachable
}

// ownSessions drops what the remote itself mirrors from other hosts: one hop only.
func ownSessions(in []webSession) []webSession {
	out := make([]webSession, 0, len(in))
	for _, s := range in {
		if s.Host == "" && !isRemoteID(s.ID) {
			out = append(out, s)
		}
	}
	return out
}

// fetchRemotes asks every remote for its state once, in parallel, each
// within timeout: `harness ls` must not wait for a host that is away.
func fetchRemotes(clients []*remoteClient, timeout time.Duration) []remoteView {
	views := make([]remoteView, len(clients))
	var wg gosync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i] = c.fetch(timeout)
		}()
	}
	wg.Wait()
	return views
}

func (c *remoteClient) fetch(timeout time.Duration) remoteView {
	v := remoteView{Name: c.name, At: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := c.request(ctx, http.MethodGet, "/api/state", nil)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	defer res.Body.Close()
	var st webState
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		v.Err = cleanNetErr(err).Error()
		return v
	}
	v.Sessions, v.At = ownSessions(st.Sessions), time.Now()
	return v
}

// remoteSession turns a remote's session into a store.Session the list
// code can lay out: the remote's name is its profile (its own group), the
// id carries the prefix. It is never written to the store.
func remoteSession(host string, ws webSession, at time.Time) store.Session {
	since := at.Add(-parseAge(ws.Age))
	if ws.Since > 0 {
		since = time.Unix(ws.Since, 0)
	}
	s := store.Session{
		ID: host + ":" + ws.ID, Profile: host, Status: ws.Status, Detail: ws.Detail,
		Cwd: ws.Dir, LastMessage: ws.Snippet,
		CreatedAt: since, UpdatedAt: since, StatusSince: since,
	}
	if ws.Label != filepath.Base(ws.Dir) {
		s.Name = ws.Label
	}
	return s
}

// remoteSessions flattens the reachable remotes into sessions.
func remoteSessions(views []remoteView) []store.Session {
	var out []store.Session
	for _, v := range views {
		if v.Err != "" {
			continue
		}
		for _, ws := range v.Sessions {
			out = append(out, remoteSession(v.Name, ws, v.At))
		}
	}
	return out
}

// parseAge reads ago()'s output back ("56s", "3m", "5h", "2d"): builds
// that do not send `since` yet only say how old a status is.
func parseAge(age string) time.Duration {
	if len(age) < 2 {
		return 0
	}
	n, err := strconv.Atoi(age[:len(age)-1])
	if err != nil || n < 0 {
		return 0
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[age[len(age)-1]]
	return time.Duration(n) * unit
}

// findRemote resolves "<remote>:<id or unique prefix>" against the
// remote's current sessions.
func findRemote(cfg *config.Config, id string) (*remoteClient, webSession, error) {
	host, rid, _ := splitRemoteID(id)
	c, err := remoteClientFor(cfg, host)
	if err != nil {
		return nil, webSession{}, err
	}
	v := c.fetch(remoteQuick)
	if v.Err != "" {
		return nil, webSession{}, fmt.Errorf("%s unreachable: %s", host, v.Err)
	}
	var found []webSession
	for _, ws := range v.Sessions {
		if ws.ID == rid {
			return c, ws, nil
		}
		if strings.HasPrefix(ws.ID, rid) {
			found = append(found, ws)
		}
	}
	switch len(found) {
	case 0:
		return nil, webSession{}, fmt.Errorf("%w: %s", store.ErrNotFound, id)
	case 1:
		return c, found[0], nil
	}
	return nil, webSession{}, fmt.Errorf("ambiguous id prefix %q", id)
}

// remoteMessages fetches the recent conversation of a remote session.
func (c *remoteClient) messages(rid string) ([]transcript.Message, error) {
	var out struct {
		Messages []transcript.Message `json:"messages"`
	}
	err := c.call(http.MethodGet, sessionPath(rid, "messages"), nil, &out)
	return out.Messages, err
}

// ---- hub: remotes kept current in the background ----

// remoteFresh is how long a remote's last state stands in for it: its
// event stream repeats the state at least every 5 s, so a few missed beats
// mean it is away.
const remoteFresh = 20 * time.Second

// remoteHub follows every remote's event stream, so the sidebar and the
// web server read remote sessions from memory and never wait on the network.
type remoteHub struct {
	clients []*remoteClient

	mu      gosync.Mutex
	views   map[string]remoteView
	raw     map[string]string // last payload per remote, to spot changes
	version int
}

func newRemoteHub(cfg *config.Config) *remoteHub {
	h := &remoteHub{clients: remoteClients(cfg), views: map[string]remoteView{}, raw: map[string]string{}}
	for _, c := range h.clients {
		v := remoteView{Name: c.name, Err: "connecting…"}
		if c.err != nil {
			v.Err = c.err.Error()
		}
		h.views[c.name] = v
	}
	return h
}

// active reports whether any remote is configured.
func (h *remoteHub) active() bool { return h != nil && len(h.clients) > 0 }

// start follows the remotes until ctx is done.
func (h *remoteHub) start(ctx context.Context) {
	for _, c := range h.clients {
		if c.err == nil {
			go h.follow(ctx, c)
		}
	}
}

func (h *remoteHub) follow(ctx context.Context, c *remoteClient) {
	for ctx.Err() == nil {
		err := h.followOnce(ctx, c)
		if ctx.Err() != nil {
			return
		}
		msg := "stream ended"
		if err != nil {
			msg = err.Error()
		}
		h.mu.Lock()
		v := h.views[c.name]
		// Keep showing the last state through a short gap (a flaky link
		// reconnects within seconds); snapshot() drops it once stale.
		if v.Err != msg {
			v.Err = msg
			h.views[c.name] = v
			h.version++
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// followOnce reads one event stream; it gives up when the remote goes
// quiet, so a connection that died without a word is replaced.
func (h *remoteHub) followOnce(ctx context.Context, c *remoteClient) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	quiet := time.AfterFunc(remoteFresh, cancel)
	defer quiet.Stop()
	err := c.sse(ctx, "/api/events", func(data []byte) {
		quiet.Reset(remoteFresh)
		var st webState
		if json.Unmarshal(data, &st) != nil {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		prev := h.views[c.name]
		h.views[c.name] = remoteView{Name: c.name, Sessions: ownSessions(st.Sessions), At: time.Now()}
		if raw := string(data); raw != h.raw[c.name] || prev.Err != "" {
			h.raw[c.name] = raw
			h.version++
		}
	})
	if errors.Is(err, context.Canceled) {
		return errors.New("timed out")
	}
	return err
}

// snapshot returns every remote in display order. A remote whose last
// state is older than remoteFresh comes back without sessions.
func (h *remoteHub) snapshot() []remoteView {
	if !h.active() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]remoteView, 0, len(h.clients))
	for _, c := range h.clients {
		v := h.views[c.name]
		switch {
		case v.At.IsZero() || time.Since(v.At) > remoteFresh:
			v.Sessions = nil
			if v.Err == "" {
				v.Err = "timed out"
			}
		default:
			v.Err = "" // a gap shorter than remoteFresh does not show
		}
		out = append(out, v)
	}
	return out
}

// stamp changes whenever snapshot() would: watchers reload on it, like on
// stateStamp.
func (h *remoteHub) stamp() string {
	if !h.active() {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "r%d", h.version)
	for _, c := range h.clients {
		v := h.views[c.name]
		if !v.At.IsZero() && time.Since(v.At) <= remoteFresh {
			b.WriteByte('+')
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (h *remoteHub) client(host string) *remoteClient {
	if h == nil {
		return nil
	}
	for _, c := range h.clients {
		if c.name == host {
			return c
		}
	}
	return nil
}

// ---- the mirror pane ----

// ensureRemotePane returns the pane of the harness tmux that mirrors the
// remote session id (`harness remote-attach`), starting it when needed.
func ensureRemotePane(cfg *config.Config, id, name string) (string, error) {
	if pane := tmux.RemotePanes()[id]; pane != "" {
		return pane, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if name == "" {
		name = shortRemote(id)
	}
	pane, err := tmux.NewWindow(cfg.TmuxSession, name, config.Expand("~"), nil,
		tmux.Quote(exe)+" remote-attach "+tmux.Quote(id))
	if err != nil {
		return "", err
	}
	tmux.MarkRemote(pane, id)
	tmux.FitToSlot(pane, cfg.SidebarWidth)
	return pane, nil
}

// withRemotePanes fills in the mirror pane of each remote session, so the
// sidebar marks and follows a shown mirror like any shown session.
func withRemotePanes(sessions []store.Session) []store.Session {
	if len(sessions) == 0 {
		return sessions
	}
	panes := tmux.RemotePanes()
	for i := range sessions {
		sessions[i].TmuxPane = panes[sessions[i].ID]
	}
	return sessions
}
