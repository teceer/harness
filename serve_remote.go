package main

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"

	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// sess is auth for a per-session call: a remote id ("<host>:<id>") is
// passed on to that host, everything else is handled here. Clients (the
// page, the native app) therefore treat remote sessions like local ones.
func (s *webServer) sess(h http.HandlerFunc) http.Handler {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if host, rid, ok := splitRemoteID(r.PathValue("id")); ok {
			s.proxy(w, r, host, sessionPath(rid, path.Base(r.URL.Path)))
			return
		}
		h(w, r)
	})
}

// proxy forwards the request to the remote's own API under its token and
// streams the answer back (SSE included). The caller's token stays here.
func (s *webServer) proxy(w http.ResponseWriter, r *http.Request, host, apiPath string) {
	c := s.hub.client(host)
	if c == nil {
		http.Error(w, "no remote named "+host, http.StatusNotFound)
		return
	}
	if c.err != nil {
		http.Error(w, c.err.Error(), http.StatusBadGateway)
		return
	}
	target, err := url.Parse(c.base + apiPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	query := r.URL.Query()
	query.Del("t")
	target.RawQuery = query.Encode()
	if r.Method != http.MethodGet {
		s.logAction(r, "%s %s on %s", r.Method, apiPath, host)
	}
	(&httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Authorization", "Bearer "+c.token)
			pr.Out.Header.Del("Cookie")
		},
		Transport:     remoteTransport,
		FlushInterval: -1, // streams (pane, conversation) pass through as they come
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, http.ErrAbortHandler) || r.Context().Err() != nil {
				return // the client went away
			}
			http.Error(w, host+" unreachable: "+cleanNetErr(err).Error(), http.StatusBadGateway)
		},
	}).ServeHTTP(w, r)
}

func (s *webServer) hubClients() []*remoteClient {
	if s.hub == nil {
		return nil
	}
	return s.hub.clients
}

// addRemotes appends the remote hosts' sessions to a snapshot: ids get the
// host prefix and the host's name becomes the profile, so they form their
// own group in every client. Numbers and "shown" are about this machine's
// terminal and do not carry over; notifications stay with the remote.
func (s *webServer) addRemotes(out *webState) {
	for _, v := range s.hub.snapshot() {
		out.Remotes = append(out.Remotes, webRemote{Name: v.Name, OK: v.Err == "", Error: v.Err})
		for _, ws := range v.Sessions {
			if ws.Since == 0 {
				ws.Since = v.At.Add(-parseAge(ws.Age)).Unix()
			}
			if ws.Unread && ws.Status != store.Archived && ws.Status != store.Ended {
				out.Unread++
			}
			ws.ID, ws.Profile, ws.Host = v.Name+":"+ws.ID, v.Name, v.Name
			ws.Number, ws.Shown = 0, false
			out.Sessions = append(out.Sessions, ws)
		}
	}
}

// newRemoteSession starts a session on a remote host ({"host": …} in
// POST /api/new); dir is a path on that machine.
func (s *webServer) newRemoteSession(w http.ResponseWriter, r *http.Request, host, dir string) {
	c := s.hub.client(host)
	if c == nil {
		http.Error(w, "no remote named "+host, http.StatusNotFound)
		return
	}
	if err := c.call(http.MethodPost, "/api/new", map[string]string{"dir": dir}, nil); err != nil {
		http.Error(w, host+": "+err.Error(), http.StatusBadGateway)
		return
	}
	s.logAction(r, "new session in %s on %s", dir, host)
	writeJSON(w, map[string]string{"ok": "1"})
}

// focusRemote shows the mirror of a remote session next to the sidebar
// (the board's click on a character).
func (s *webServer) focusRemote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	host, _, _ := splitRemoteID(id)
	if s.hub.client(host) == nil {
		http.Error(w, "no remote named "+host, http.StatusNotFound)
		return
	}
	sb := tmux.Sidebar()
	if sb == "" {
		http.Error(w, "sidebar is not running", http.StatusConflict)
		return
	}
	pane, err := ensureRemotePane(s.cfg, id, "")
	if err == nil {
		var all []store.Session
		if all, err = s.st.List(); err == nil {
			err = showSession(s.cfg, sb, all, store.Session{TmuxPane: pane})
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.logAction(r, "focus %s", shortRemote(id))
	writeJSON(w, map[string]string{"ok": "1"})
}
