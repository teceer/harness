package main

import (
	tea "github.com/charmbracelet/bubbletea"
	"testing"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
)

func TestSelectionFollowsNewPane(t *testing.T) {
	m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
	now := time.Now()
	a := store.Session{ID: "a", Profile: "personal", Status: store.Idle, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	m.Update(dataMsg{sessions: []store.Session{a}})
	if m.selID != "a" {
		t.Fatalf("initial selection %q", m.selID)
	}
	m.selPane, m.selID = "%2", "" // what opMsg{selectPane} does
	b := store.Session{ID: "pending-%2", Profile: "personal", Status: store.Starting, TmuxPane: "%2", CreatedAt: now, UpdatedAt: now}
	m.Update(dataMsg{sessions: []store.Session{a, b}})
	if m.selID != "pending-%2" {
		t.Fatalf("selection = %q, want pending-%%2", m.selID)
	}
}

func TestSelectionSurvivesPlaceholderRename(t *testing.T) {
	m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
	now := time.Now()
	a := store.Session{ID: "a", Profile: "personal", Status: store.Idle, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	b := store.Session{ID: "pending-%2", Profile: "personal", Status: store.Starting, TmuxPane: "%2", CreatedAt: now, UpdatedAt: now}
	m.selPane = "%2"
	m.Update(dataMsg{sessions: []store.Session{a, b}})
	b.ID, b.Status = "real-id", store.Idle
	m.Update(dataMsg{sessions: []store.Session{a, b}})
	if m.selID != "real-id" {
		t.Fatalf("selection = %q, want real-id", m.selID)
	}
}

func TestSectionsSplitByHostAndTab(t *testing.T) {
	now := time.Now()
	mk := func(id, profile, status, pane string, age time.Duration) store.Session {
		return store.Session{ID: id, Profile: profile, Status: status, TmuxPane: pane,
			CreatedAt: now.Add(-age), UpdatedAt: now.Add(-age)}
	}
	all := []store.Session{
		mk("h2", "work", store.Idle, "%2", time.Minute),
		mk("h1", "personal", store.Working, "%1", 2*time.Minute),
		mk("t1", "personal", store.Idle, "", time.Minute), // plain terminal tab
		mk("ar", "work", store.Archived, "", time.Hour),
		mk("en", "personal", store.Ended, "", time.Hour),
		mk("old", "personal", store.Ended, "", 48*time.Hour),
	}
	ids := func(ss []store.Session) (out []string) {
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return
	}
	sess := sections(all, tabSessions, false, now)
	if got := ids(sess[0].sessions); len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Errorf("active = %v, want [h1 h2] (personal before work)", got)
	}
	if got := ids(sess[1].sessions); len(got) != 1 || got[0] != "t1" {
		t.Errorf("outside = %v, want [t1]", got)
	}
	arch := sections(all, tabArchived, false, now)
	if got := ids(flatten(arch)); len(got) != 2 || got[0] != "ar" || got[1] != "en" {
		t.Errorf("archived tab = %v, want [ar en]", got)
	}
	if got := flatten(sections(all, tabArchived, true, now)); len(got) != 3 {
		t.Errorf("showEnded should reveal old ended sessions, got %d", len(got))
	}

	order := switchOrder(all, now)
	if n, _ := neighbour(order, "%1", 1); n.ID != "h2" {
		t.Errorf("next after h1 = %s", n.ID)
	}
	if n, _ := neighbour(order, "%1", -1); n.ID != "h2" {
		t.Errorf("prev of first should wrap to h2, got %s", n.ID)
	}
	if n, _ := neighbour(order, "%9", 1); n.ID != "h1" {
		t.Errorf("unknown current should start at first, got %s", n.ID)
	}
}

func TestSelectionRememberedPerTab(t *testing.T) {
	now := time.Now()
	m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
	m.Update(dataMsg{sessions: []store.Session{
		{ID: "a", Profile: "p", Status: store.Idle, TmuxPane: "%1", CreatedAt: now, UpdatedAt: now},
		{ID: "b", Profile: "p", Status: store.Idle, TmuxPane: "%2", CreatedAt: now.Add(time.Second), UpdatedAt: now},
		{ID: "z", Profile: "p", Status: store.Archived, CreatedAt: now, UpdatedAt: now},
	}})
	m.move(1)
	if m.selID != "b" {
		t.Fatalf("sel = %s", m.selID)
	}
	m.setTab(tabArchived)
	if m.selID != "z" {
		t.Fatalf("archived tab sel = %s", m.selID)
	}
	m.setTab(tabSessions)
	if m.selID != "b" {
		t.Fatalf("selection not restored: %s", m.selID)
	}
}

func TestMoveConfirmDefaultsToYes(t *testing.T) {
	now := time.Now()
	ext := store.Session{ID: "ext", Profile: "p", Status: store.Idle, PID: 1, CreatedAt: now, UpdatedAt: now}
	for key, wantMove := range map[string]bool{"enter": true, "y": true, "n": false, "esc": false} {
		m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
		m.Update(dataMsg{sessions: []store.Session{ext}})
		m.Update(askMoveMsg{id: "ext"})
		var msg tea.KeyMsg
		switch key {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		_, cmd := m.Update(msg)
		if moved := cmd != nil; moved != wantMove {
			t.Errorf("key %q: move started = %v, want %v", key, moved, wantMove)
		}
		if m.confirmMove != "" {
			t.Errorf("key %q: prompt still open", key)
		}
	}
}

func TestLeavingSidebarSelectsShownSession(t *testing.T) {
	now := time.Now()
	m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
	m.Update(dataMsg{shown: "%2", sessions: []store.Session{
		{ID: "a", Profile: "p", Status: store.Idle, TmuxPane: "%1", CreatedAt: now, UpdatedAt: now},
		{ID: "b", Profile: "p", Status: store.Idle, TmuxPane: "%2", CreatedAt: now.Add(time.Second), UpdatedAt: now},
		{ID: "z", Profile: "p", Status: store.Archived, CreatedAt: now, UpdatedAt: now},
	}})
	m.moveTo(0) // user wandered off to "a"
	m.setTab(tabArchived)
	m.Update(tea.BlurMsg{})
	if m.tab != tabSessions || m.selID != "b" {
		t.Fatalf("after blur: tab=%d sel=%s, want Sessions/b", m.tab, m.selID)
	}
}

func TestPeekKeepsSelection(t *testing.T) {
	now := time.Now()
	m := newSidebar(&config.Config{SidebarWidth: 42}, nil, "%0")
	m.Update(dataMsg{shown: "%2", sessions: []store.Session{
		{ID: "a", Profile: "p", Status: store.Idle, TmuxPane: "%1", CreatedAt: now, UpdatedAt: now},
		{ID: "b", Profile: "p", Status: store.Idle, TmuxPane: "%2", CreatedAt: now.Add(time.Second), UpdatedAt: now},
	}})
	m.moveTo(0)      // selected "a", "b" is shown on the right
	m.peeking = true // what peek() sets before the popup opens
	m.Update(tea.BlurMsg{})
	m.Update(peekDoneMsg{})
	if m.selID != "a" {
		t.Fatalf("peek moved the selection to %s", m.selID)
	}
}
