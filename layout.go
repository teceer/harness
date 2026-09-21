package main

import (
	"sort"
	"time"

	"github.com/teceer/harness/internal/store"
)

type tab int

const (
	tabSessions tab = iota
	tabArchived
)

var tabNames = [...]string{"Sessions", "Archived"}

// section is a titled block of the sidebar list.
type section struct {
	title    string
	sessions []store.Session
}

// endedVisible keeps finished (not archived) sessions in the Archived tab
// for a while; `.` shows all of them.
const endedVisible = 24 * time.Hour

// sections lays out what a tab lists, in display order. The Sessions tab
// splits running sessions into those hosted by harness (switchable,
// archivable) and those living in plain terminal tabs; the Archived tab
// holds everything stopped and resumable.
func sections(all []store.Session, t tab, showEnded bool, now time.Time) []section {
	var a, b []store.Session
	for _, s := range all {
		switch {
		case t == tabSessions && s.Live() && s.TmuxPane != "":
			a = append(a, s)
		case t == tabSessions && s.Live():
			b = append(b, s)
		case t == tabArchived && s.Status == store.Archived:
			a = append(a, s)
		case t == tabArchived && s.Status == store.Ended && (showEnded || now.Sub(s.UpdatedAt) < endedVisible):
			b = append(b, s)
		}
	}
	if t == tabSessions {
		byProfile(a, true)
		byProfile(b, true)
		return []section{{"active", a}, {"outside harness", b}}
	}
	byProfile(a, false)
	byProfile(b, false)
	return []section{{"archived", a}, {"ended", b}}
}

// byProfile groups by profile ("other" last). Running sessions keep their
// creation order so rows do not jump around; stopped ones show newest first.
func byProfile(ss []store.Session, running bool) {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.Profile != b.Profile {
			if (a.Profile == "other") != (b.Profile == "other") {
				return b.Profile == "other"
			}
			return a.Profile < b.Profile
		}
		if running {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.UpdatedAt.After(b.UpdatedAt)
	})
}

func flatten(secs []section) []store.Session {
	var out []store.Session
	for _, s := range secs {
		out = append(out, s.sessions...)
	}
	return out
}

// switchOrder is the cycle ⌘[ / ⌘] walks: the harness-hosted sessions in
// sidebar order.
func switchOrder(all []store.Session, now time.Time) []store.Session {
	return sections(all, tabSessions, false, now)[0].sessions
}

// neighbour returns the session after (delta=1) or before (delta=-1) the
// one in pane current, wrapping around; the first one if current is unknown.
func neighbour(order []store.Session, current string, delta int) (store.Session, bool) {
	if len(order) == 0 {
		return store.Session{}, false
	}
	for i, s := range order {
		if s.TmuxPane == current {
			return order[(i+delta+len(order))%len(order)], true
		}
	}
	return order[0], true
}
