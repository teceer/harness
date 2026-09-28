package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/teceer/harness/internal/store"
)

// Read state, so nothing slips by: a session is unread when it finished a
// turn or started waiting after you last opened it, or when you marked it
// unread yourself. It lives on the server (~/.harness/read-state.json), so
// the app, the page and every device agree.

type readState struct {
	// Since is when tracking started: earlier finishes count as read, so
	// turning this on does not flag every old session at once.
	Since  time.Time            `json:"since"`
	Seen   map[string]time.Time `json:"seen"`
	Marked map[string]bool      `json:"marked"` // marked unread by hand
}

func (s *webServer) readPath() string { return filepath.Join(s.cfg.Home, "read-state.json") }

// loadRead returns the read state; call with s.readMu held.
func (s *webServer) loadRead() readState {
	rs := readState{Seen: map[string]time.Time{}, Marked: map[string]bool{}}
	if b, err := os.ReadFile(s.readPath()); err == nil {
		json.Unmarshal(b, &rs)
	}
	if rs.Seen == nil {
		rs.Seen = map[string]time.Time{}
	}
	if rs.Marked == nil {
		rs.Marked = map[string]bool{}
	}
	if rs.Since.IsZero() {
		rs.Since = time.Now()
		s.saveRead(rs)
	}
	return rs
}

func (s *webServer) saveRead(rs readState) error {
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.readPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.readPath()); err != nil {
		return err
	}
	bump(s.cfg) // live clients refresh at once
	return nil
}

// unread tells whether se has news you have not opened.
func (rs readState) unread(se store.Session) bool {
	if rs.Marked[se.ID] {
		return true
	}
	if se.Status != store.Idle && se.Status != store.Waiting {
		return false // working: nothing to read yet
	}
	seen, ok := rs.Seen[se.ID]
	if !ok {
		seen = rs.Since
	}
	return se.StatusSince.Unix() > seen.Unix()
}

// unreadCount is the number for the app icon badge.
func (s *webServer) unreadCount() int {
	all, err := s.st.List()
	if err != nil {
		return 0
	}
	s.readMu.Lock()
	rs := s.loadRead()
	s.readMu.Unlock()
	n := 0
	for _, x := range all {
		if x.Live() && rs.unread(x) {
			n++
		}
	}
	return n
}

// markRead: POST /api/sessions/{id}/read (opened) and …/unread (by hand).
func (s *webServer) markRead(unread bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se, err := s.st.Find(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.readMu.Lock()
		rs := s.loadRead()
		if unread {
			rs.Marked[se.ID] = true
		} else {
			delete(rs.Marked, se.ID)
			// Opening covers whatever the session did so far, even if its
			// status time is ahead of this clock (stored in whole seconds).
			seen := time.Now()
			if se.StatusSince.After(seen) {
				seen = se.StatusSince
			}
			rs.Seen[se.ID] = seen
		}
		err = s.saveRead(rs)
		s.readMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": "1", "unread": s.unreadCount()})
	}
}
