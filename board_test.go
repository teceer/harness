package main

import (
	"testing"
	"time"

	"github.com/teceer/harness/internal/store"
)

func TestScanLineCountsEachMessageOnce(t *testing.T) {
	sc := &transcriptScan{seen: map[string]bool{}}
	msg := `{"type":"user","timestamp":"2026-11-10T09:00:00.000Z","message":{"content":"<cross-session-message from=\"uds:/tmp/cc-socks/3379.sock\" from-name=\"a\">t71 gotowy</cross-session-message>"}}`
	scanLine(sc, msg+"\n")
	scanLine(sc, msg+"\n") // echoed again later
	scanLine(sc, `{"type":"api-request-blob","x":"<cross-session-message from=\"uds:/tmp/cc-socks/1.sock\">"}`+"\n")
	if len(sc.msgs) != 1 || sc.msgs[0].fromPID != 3379 {
		t.Fatalf("msgs = %+v", sc.msgs)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-11-10T09:00:00Z"); !sc.msgs[0].at.Equal(want) {
		t.Fatalf("at = %v", sc.msgs[0].at)
	}
}

func TestScanLineHandoffParentOnlyAtStart(t *testing.T) {
	sc := &transcriptScan{seen: map[string]bool{}}
	scanLine(sc, `{"type":"user","message":{"content":"przeczytaj /Users/x/.claude/handoffs/a03549bb-b4fb-4a6d-8caf-47cfae9cc6d8.md i kontynuuj"}}`+"\n")
	if sc.parent != "a03549bb-b4fb-4a6d-8caf-47cfae9cc6d8" {
		t.Fatalf("parent = %q", sc.parent)
	}
	late := &transcriptScan{seen: map[string]bool{}}
	for i := 0; i < 7; i++ {
		scanLine(late, `{"type":"user","message":{"content":"hej"}}`+"\n")
	}
	scanLine(late, `{"type":"user","message":{"content":"przeczytaj /h/handoffs/a03549bb-b4fb-4a6d-8caf-47cfae9cc6d8.md"}}`+"\n")
	if late.parent != "" {
		t.Fatalf("late parent = %q", late.parent)
	}
}

func TestLinkEdgesFollowsHandoffsAndPIDs(t *testing.T) {
	now := time.Now()
	all := []store.Session{
		{ID: "pm-new", PID: 100, Status: store.Idle, TranscriptPath: "pm-new", UpdatedAt: now},
		{ID: "pm-old", Status: store.Ended, TranscriptPath: "pm-old", UpdatedAt: now},
		{ID: "dev", PID: 200, Status: store.Working, TranscriptPath: "dev", UpdatedAt: now},
	}
	scans := map[string]*transcriptScan{
		"pm-new": {parent: "pm-old", msgs: []linkMsg{{fromPID: 200, at: now}}},
		"pm-old": {msgs: []linkMsg{{fromPID: 200, at: now}, {fromPID: 200, at: now.Add(-48 * time.Hour)}, {fromPID: 999, at: now}}},
		"dev":    {msgs: []linkMsg{{fromPID: 100, at: now}}},
	}
	edges := linkEdges(all, now.Add(-24*time.Hour), func(p string) *transcriptScan { return scans[p] })
	got := map[string]int{}
	for _, e := range edges {
		got[e.From+">"+e.To] = e.Count
	}
	if got["dev>pm-new"] != 2 || got["pm-new>dev"] != 1 || len(got) != 2 {
		t.Fatalf("edges = %v", got)
	}
}

func TestLiveKeysAllowOnlyKeyboardKeys(t *testing.T) {
	for _, k := range []string{"Enter", "Escape", "BSpace", "BTab", "Up", "M-Enter", "C-c", "C-r", "DC", "PageUp"} {
		if !liveKeyRe.MatchString(k) {
			t.Errorf("%q should be allowed", k)
		}
	}
	for _, k := range []string{"", "C-C", "C-cc", "MouseDown1Pane", "Enter;kill-server", "-X", "C-\\", "q Enter"} {
		if liveKeyRe.MatchString(k) {
			t.Errorf("%q should be refused", k)
		}
	}
}
