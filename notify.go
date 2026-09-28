package main

import (
	"errors"
	"flag"
	"os"
	"strings"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// cmdNotify sends a push notification to the harness app — how agents
// report progress and finished work (Telegram is kept for alerts and
// decisions). Run from a session in the harness tmux, it names that
// session and tapping the notification opens it.
//
//	harness notify [-t title] [-s session] message…
func cmdNotify(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	title := fs.String("t", "", "title (default: the session's name)")
	id := fs.String("s", "", "session id or prefix (default: the calling session)")
	fs.Parse(args)
	body := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if body == "" {
		return errors.New("usage: harness notify [-t title] [-s session] message")
	}

	return withStore(func(cfg *config.Config, st *store.Store) error {
		srv := &webServer{cfg: cfg, st: st}
		if len(srv.pushStore().list()) == 0 {
			return errors.New("no device registered for push: log in to the harness app first")
		}
		se, found := callingSession(st, *id)
		if *title == "" {
			*title = "Harness"
			if found {
				*title = label(se)
			}
		}
		if !found {
			se = store.Session{} // no link: the app opens on the list
		}
		se.Status = "" // an update, not a prompt: no answer buttons
		return srv.deliverPush(se, *title, body)
	})
}

// callingSession finds the session to link: -s when given, otherwise the
// one whose pane this runs in (HARNESS_PANE is set for every session
// harness starts; TMUX_PANE when run inside the harness server itself).
func callingSession(st *store.Store, id string) (store.Session, bool) {
	if id != "" {
		se, err := st.Find(id)
		return se, err == nil
	}
	pane := os.Getenv("HARNESS_PANE")
	if pane == "" && tmux.Inside() {
		pane = os.Getenv("TMUX_PANE")
	}
	if pane == "" {
		return store.Session{}, false
	}
	all, err := st.List()
	if err != nil {
		return store.Session{}, false
	}
	for _, x := range all {
		if x.TmuxPane == pane && x.Live() {
			return x, true
		}
	}
	return store.Session{}, false
}
