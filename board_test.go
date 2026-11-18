package main

import (
	"strings"
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

func TestScanAssistantReadsModelToolAndTokens(t *testing.T) {
	sc := &transcriptScan{seen: map[string]bool{}}
	scanLine(sc, `{"type":"assistant","timestamp":"2026-11-11T10:05:00.000Z","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}],"usage":{"input_tokens":2,"cache_creation_input_tokens":20000,"cache_read_input_tokens":30000,"output_tokens":100,"output_tokens_details":{"thinking_tokens":13}}}}`+"\n")
	scanLine(sc, `{"type":"assistant","isSidechain":true,"timestamp":"2026-11-11T10:06:00.000Z","message":{"model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}],"usage":{"input_tokens":5000,"output_tokens":50}}}`+"\n")
	scanLine(sc, `{"type":"assistant","timestamp":"2026-11-11T11:00:00.000Z","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":50000,"output_tokens":20}}}`+"\n")
	scanLine(sc, `{"type":"api-request-blob","x":{"type":"assistant","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":999999,"output_tokens":1}}}}`+"\n")
	if sc.model != "claude-opus-5-5" || sc.tool != "Bash" {
		t.Fatalf("model=%q tool=%q (sidechains must not win)", sc.model, sc.tool)
	}
	if sc.ctx != 50230 {
		t.Fatalf("ctx = %d, want the last main turn", sc.ctx)
	}
	at, _ := time.Parse(time.RFC3339, "2026-11-11T10:30:00Z")
	if u := sc.usedSince(at); u.In != 50002+5000+50210 || u.Out != 170 {
		t.Fatalf("used since 10:30 = %+v (hour buckets)", u)
	}
	if u := sc.usedSince(at.Add(time.Hour)); u.In != 50210 || u.Out != 20 {
		t.Fatalf("used since 11:30 = %+v", u)
	}
}

func TestBoardStatsOnlyLiveSessionsWithData(t *testing.T) {
	all := []store.Session{
		{ID: "a", Status: store.Working, TranscriptPath: "a"},
		{ID: "b", Status: store.Ended, TranscriptPath: "b"},
		{ID: "c", Status: store.Idle, TranscriptPath: "c"},
	}
	scans := map[string]*transcriptScan{
		"a": {model: "claude-opus-5-5", ctx: 1000, used: map[int64]tokens{time.Now().Unix() / 3600: {In: 10, Out: 5}}},
		"b": {model: "claude-opus-5-5", ctx: 1000},
		"c": {},
	}
	got := boardStats(all, time.Now().Add(-time.Hour), func(p string) *transcriptScan { return scans[p] })
	if len(got) != 1 || got["a"].Model != "claude-opus-5-5" || got["a"].Ctx != 1000 || got["a"].Used.In != 10 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestSayLine(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"Gotowe.": "Gotowe.",
		"Zadanie t153 jest gotowe: PR #1484 jest scalony (`2d0e51079`), a jedną linię z pliku poprawiłem. Dalej idę.":                 "Zadanie t153 jest gotowe: PR #1484 jest scalony, a jedną linię z pliku…",
		"**Przekazanie** zapisałem w `/Users/x/.claude/handoffs/96811cec.md` i czekam.":                                               "Przekazanie zapisałem w i czekam.",
		"Utworzyłem plik `forwarded.txt` w katalogu `scratchpad/relay-test` z treścią ok":                                             "Utworzyłem plik w katalogu z treścią ok",
		"Zobacz https://example.com/a/b?x=1 i [dokument](https://x.y/z).":                                                             "Zobacz i dokument.",
		"# Nagłówek\n\n- pierwszy punkt\n- drugi punkt":                                                                               "Nagłówek pierwszy punkt drugi punkt",
		"Np. to działa w 1.5 s. A potem nie.":                                                                                         "Np. to działa w 1.5 s.",
		"```go\nfunc main() {}\n```\nKod skompilowany!":                                                                               "Kod skompilowany!",
		"<task-notification>\n<task-id>bvcy4pw9o</task-id>\n<status>completed</status>\nAgent skończył recenzję.</task-notification>": "bvcy4pw9o completed Agent skończył recenzję.",
		"> napraw testy w module płatności":                                                                                           "napraw testy w module płatności",
		"W toku są dwa zadania w tle:\n- druga recenzja #1480;\n- sprawdzenie mutantów":                                               "W toku są dwa zadania w tle: druga recenzja #1480; sprawdzenie mutantów",
		strings.Repeat("bardzo długie zdanie bez kropki ", 6):                                                                         "bardzo długie zdanie bez kropki bardzo długie zdanie bez kropki bardzo długie…",
	}
	for in, want := range cases {
		if got := sayLine(in); got != want {
			t.Errorf("sayLine(%q)\n got %q\nwant %q", in, got, want)
		}
		if n := len([]rune(sayLine(in))); n > sayMax {
			t.Errorf("sayLine(%q) is %d runes", in, n)
		}
	}
}

func TestSayForPicksTheRightSource(t *testing.T) {
	s := store.Session{Status: store.Waiting, Detail: "Claude needs your permission to use Bash", LastPrompt: "zrób x", LastMessage: "Zrobione."}
	if got := sayFor(s); got != "Claude needs your permission to use Bash" {
		t.Fatalf("waiting: %q", got)
	}
	s.Status = store.Working
	if got := sayFor(s); got != "zrób x" {
		t.Fatalf("working: %q", got)
	}
	s.Status = store.Idle
	if got := sayFor(s); got != "Zrobione." {
		t.Fatalf("idle: %q", got)
	}
}

func TestPRNumber(t *testing.T) {
	if n := prNumber(store.Session{Name: "[DEV] t138 #1480", LastMessage: "PR #1 scalony"}); n != 1480 {
		t.Fatalf("name wins: %d", n)
	}
	if n := prNumber(store.Session{Name: "[DEV] t134", LastMessage: "t137 (#1476) jest scalony"}); n != 1476 {
		t.Fatalf("message: %d", n)
	}
	if n := prNumber(store.Session{Name: "[PM] core11", LastMessage: "krok #3 gotowy"}); n != 0 {
		t.Fatalf("one digit is not a PR: %d", n)
	}
}
