package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teceer/harness/internal/store"
)

func TestScanCountsAMessageOnce(t *testing.T) {
	sc := &transcriptScan{seen: map[string]bool{}}
	line := func(id, block string) string {
		return `{"type":"assistant","timestamp":"2026-11-12T10:05:30.000Z","message":{"id":"` + id + `","model":"claude-opus-5-5","content":[` + block + `],"usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":40}}}` + "\n"
	}
	// One message written as three lines (thinking, text, tool call), then another.
	scanLine(sc, line("msg_A", `{"type":"thinking"}`))
	scanLine(sc, line("msg_A", `{"type":"text","text":"hej"}`))
	scanLine(sc, line("msg_A", `{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}`))
	scanLine(sc, line("msg_B", `{"type":"text","text":"ok"}`))

	min := time.Date(2026, 11, 12, 10, 5, 0, 0, time.UTC).Unix() / 60
	if got := sc.perMin[min]; got != (kpiTokens{In: 206, Cache: 10000, Out: 80}) {
		t.Fatalf("minute bucket = %+v, want two messages", got)
	}
	if u := sc.usedSince(time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)); u.In != 2*5103 || u.Out != 80 {
		t.Fatalf("hour bucket = %+v, want two messages", u)
	}
	if sc.tool != "Bash" {
		t.Fatalf("tool = %q", sc.tool)
	}
}

func TestTokenRates(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 30, 0, time.UTC)
	m := now.Unix() / 60
	a := &transcriptScan{perMin: map[int64]kpiTokens{
		m:      {In: 999999, Cache: 0, Out: 9999}, // unfinished minute: in neither
		m - 15: {In: 150, Cache: 1500, Out: 15},   // oldest minute of 15 min
		m - 14: {In: 150, Cache: 0, Out: 15},      // in 15 min
		m - 16: {In: 3000, Cache: 0, Out: 300},    // only in 60 min
		m - 61: {In: 999999, Cache: 0, Out: 9999}, // in neither
	}}
	sub := &transcriptScan{perMin: map[int64]kpiTokens{m - 1: {In: 0, Cache: 0, Out: 15}}}
	got := tokenRates([]*transcriptScan{a, sub}, now, 15, 60)
	if len(got) != 2 {
		t.Fatalf("got %d windows", len(got))
	}
	if got[0].Minutes != 15 || got[0].Sum != (kpiTokens{In: 300, Cache: 1500, Out: 45}) || got[0].PerMin != (kpiTokens{In: 20, Cache: 100, Out: 3}) {
		t.Fatalf("15 min = %+v", got[0])
	}
	if got[1].Sum != (kpiTokens{In: 3300, Cache: 1500, Out: 345}) || got[1].PerMin.In != 55 {
		t.Fatalf("60 min = %+v", got[1])
	}
	if r := tokenRates(nil, now, 15); r[0].Sum != (kpiTokens{}) {
		t.Fatalf("no scans = %+v", r)
	}
}

func TestPrunePerMin(t *testing.T) {
	now := time.Now()
	m := now.Unix() / 60
	sc := &transcriptScan{perMin: map[int64]kpiTokens{m: {Out: 1}, m - kpiKeep - 1: {Out: 1}}}
	sc.prunePerMin(now)
	if len(sc.perMin) != 1 {
		t.Fatalf("kept %v", sc.perMin)
	}
}

func TestRecentTranscriptsIncludeSubagents(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s1.jsonl")
	sub := filepath.Join(dir, "s1", "subagents", "agent-a.jsonl")
	old := filepath.Join(dir, "s1", "subagents", "agent-old.jsonl")
	os.MkdirAll(filepath.Dir(sub), 0o755)
	for _, p := range []string{main, sub, old} {
		os.WriteFile(p, []byte("{}\n"), 0o644)
	}
	long := time.Now().Add(-3 * time.Hour)
	os.Chtimes(old, long, long)
	now := time.Now()
	all := []store.Session{
		{ID: "s1", TranscriptPath: main, UpdatedAt: now},
		{ID: "s2", TranscriptPath: filepath.Join(dir, "s2.jsonl"), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "s3"},
	}
	got := recentTranscripts(all, now.Add(-time.Hour))
	if len(got) != 2 || got[0] != main || got[1] != sub {
		t.Fatalf("got %v", got)
	}
}

func TestParseUsage(t *testing.T) {
	data := []byte(`{"at":1762941600,"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1762950000},"seven_day":{"used_percentage":71,"resets_at":"2026-11-15T08:00:00Z"}}}`)
	u, err := parseUsage("claude", data, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if u.Account != "claude" || u.At != "2025-11-12T10:00:00Z" {
		t.Fatalf("got %+v", u)
	}
	if u.FiveHour == nil || u.FiveHour.Pct != 42.5 || u.FiveHour.ResetAt != "2025-11-12T12:20:00Z" {
		t.Fatalf("five hour = %+v", u.FiveHour)
	}
	if u.Week == nil || u.Week.Pct != 71 || u.Week.ResetAt != "2026-11-15T08:00:00Z" {
		t.Fatalf("week = %+v", u.Week)
	}

	// Milliseconds, a missing week, no "at" (the file's time is used).
	mod := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	u, err = parseUsage("claude-epc", []byte(`{"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1762950000000}}}`), mod)
	if err != nil || u.Week != nil || u.FiveHour.ResetAt != "2025-11-12T12:20:00Z" || u.At != "2026-11-12T09:00:00Z" {
		t.Fatalf("got %+v %v", u, err)
	}

	for _, bad := range []string{`{}`, `{"rate_limits":{}}`, `nope`} {
		if _, err := parseUsage("x", []byte(bad), mod); err == nil {
			t.Errorf("%s should be no data", bad)
		}
	}
}

func TestReadUsage(t *testing.T) {
	dir := t.TempDir()
	if _, err := readUsage(dir); err == nil {
		t.Fatal("no file should be an error (brak danych)")
	}
	os.WriteFile(filepath.Join(dir, "usage-claude.json"), []byte(`{"at":1,"rate_limits":{"five_hour":{"used_percentage":10}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "usage-claude-epc.json"), []byte(`broken`), 0o644)
	got, err := readUsage(dir)
	if err != nil || len(got) != 1 || got[0].Account != "claude" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestPRStats(t *testing.T) {
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	h := func(x float64) time.Time { return now.Add(time.Duration(x * float64(time.Hour))) }
	prs := []kpiPR{
		{Number: 1, FirstCommit: h(-5), Opened: h(-4), Merged: h(-3)},    // lead 2 h, open 1 h
		{Number: 2, FirstCommit: h(-10), Opened: h(-9.5), Merged: h(-6)}, // lead 4 h, open 3.5 h
		{Number: 3, FirstCommit: h(-2), Opened: h(-3), Merged: h(-1)},    // opened first: lead 2 h, open 2 h
		{Number: 4, Opened: h(-1), Merged: h(-0.5)},                      // no commits: lead 0.5 h
		{Number: 5, FirstCommit: h(-40), Opened: h(-30), Merged: h(-25)}, // merged before the window
	}
	got := prStats(prs, []string{"teceer/harness"}, now)
	if got.Count != 4 {
		t.Fatalf("count = %d", got.Count)
	}
	if got.MedianLead != 120 || got.MeanLead != (120+240+120+30)/4.0 {
		t.Fatalf("lead median %v mean %v", got.MedianLead, got.MeanLead)
	}
	if got.MedianOpen != (60+120)/2.0 {
		t.Fatalf("open median %v", got.MedianOpen)
	}
	if empty := prStats(nil, nil, now); empty.Count != 0 || empty.MedianLead != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestParseGhPRs(t *testing.T) {
	data := []byte(`[{"data":{"search":{"pageInfo":{"hasNextPage":true},"nodes":[
		{"number":7,"createdAt":"2026-11-12T10:00:00Z","mergedAt":"2026-11-12T12:00:00Z","commits":{"nodes":[{"commit":{"authoredDate":"2026-11-12T08:00:00Z","committedDate":"2026-11-12T09:45:00Z"}}]}},
		{}]}}},
		{"data":{"search":{"nodes":[{"number":8,"createdAt":"2026-11-12T10:00:00Z","mergedAt":"2026-11-12T11:00:00Z","commits":{"nodes":[]}}]}}}]`)
	got, err := parseGhPRs("o/r", data)
	if err != nil || len(got) != 2 || got[1].Number != 8 || !got[1].FirstCommit.IsZero() {
		t.Fatalf("got %v %v", got, err)
	}
	if p := got[0]; p.Repo != "o/r" || p.Number != 7 || !p.FirstCommit.Equal(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v", p)
	}
	if _, err := parseGhPRs("o/r", []byte("gh: not logged in")); err == nil {
		t.Fatal("garbage should be an error")
	}
}

func TestGithubRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:teceer/harness.git\n":    "teceer/harness",
		"https://github.com/teceer/core11":       "teceer/core11",
		"https://github.com/teceer/core11.git/":  "teceer/core11",
		"ssh://git@github.com/a-b/c.d.git":       "a-b/c.d",
		"git@gitlab.com:epc/traffico-portal.git": "",
		"":                                       "",
	} {
		if got := githubRepo(in); got != want {
			t.Errorf("githubRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMedian(t *testing.T) {
	if median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 3, 2}) != 2.5 || median(nil) != 0 {
		t.Fatal("median")
	}
}
