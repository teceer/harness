package main

import (
	"testing"
	"time"

	"github.com/teceer/harness/internal/store"
)

func TestLiveness(t *testing.T) {
	now := time.Now()
	h := liveness{panes: map[string]int{"%3": 1}, claude: map[int]bool{100: true}}
	cases := []struct {
		name string
		s    store.Session
		want bool
	}{
		{"pane and pid alive", store.Session{Status: store.Idle, TmuxPane: "%3", PID: 100}, true},
		{"pane gone, pid alive", store.Session{Status: store.Idle, TmuxPane: "%1", PID: 100}, false},
		{"pane alive, pid gone", store.Session{Status: store.Idle, TmuxPane: "%3", PID: 200}, false},
		{"terminal tab, pid alive", store.Session{Status: store.Idle, PID: 100}, true},
		{"terminal tab, pid gone", store.Session{Status: store.Idle, PID: 200}, false},
		{"starting, pane not yet listed", store.Session{Status: store.Starting, TmuxPane: "%9", StatusSince: now}, true},
		{"starting too long", store.Session{Status: store.Starting, TmuxPane: "%9", StatusSince: now.Add(-time.Minute)}, false},
	}
	for _, c := range cases {
		if got := h.alive(c.s, now); got != c.want {
			t.Errorf("%s: alive = %v, want %v", c.name, got, c.want)
		}
	}
}
