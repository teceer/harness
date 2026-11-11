package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/teceer/harness/internal/sysstat"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"
	"unicode"

	"github.com/teceer/harness/internal/ansi"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// The flow board (/board): every agent as a card with its live terminal and
// a prompt box, plus who talks to whom, counted from the cross-session
// messages in the transcripts.

func (s *webServer) boardPage(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/board.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

// maxBoardPanes bounds one multiplexed stream: a board shows a few dozen
// cards, and each capture is a tmux call.
const maxBoardPanes = 64

// panesStream mirrors many panes over one SSE connection (a browser allows
// only six HTTP/1.1 connections per host, so one stream per card would
// starve the page). ?ids=a,b,c; each event is {"id", "html"} for a pane
// whose screen changed, with the pane size in cells ("cols", "rows"). ?focus=<id> is captured fast, the rest once a second.
func (s *webServer) panesStream(w http.ResponseWriter, r *http.Request) {
	type target struct{ id, pane, last string }
	var targets []*target
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id == "" || len(targets) >= maxBoardPanes {
			continue
		}
		if se, err := s.st.Find(id); err == nil && se.TmuxPane != "" {
			targets = append(targets, &target{id: se.ID, pane: se.TmuxPane, last: "\x00"})
		}
	}
	focus := r.URL.Query().Get("focus")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()
	for tick := 0; ; tick++ {
		for _, t := range targets {
			fast := focus != "" && strings.HasPrefix(t.id, focus)
			if !fast && tick%3 != 0 {
				continue
			}
			raw, cols, rows, err := tmux.CaptureGrid(t.pane)
			sig := fmt.Sprintf("%dx%d\x00%s", cols, rows, raw)
			if err != nil || sig == t.last {
				continue
			}
			t.last = sig
			data, _ := json.Marshal(map[string]any{"id": t.id, "html": ansi.HTML(raw), "cols": cols, "rows": rows})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(350 * time.Millisecond):
		}
	}
}

// ---- links: who talks to whom ----

type boardEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
	Last  string `json:"last"` // RFC 3339
}

// transcriptScan is what one transcript says about links, read incrementally:
// a transcript only grows, so the next scan starts where this one stopped.
type transcriptScan struct {
	offset int64
	users  int       // user entries read so far
	parent string    // session this one was resumed from (handoff after /clear)
	msgs   []linkMsg // cross-session messages received
	seen   map[string]bool

	// For the 8-bit board, read from the same pass over the lines.
	model string           // model of the last assistant turn
	ctx   int              // context the last turn was sent with (tokens)
	tool  string           // name of the last tool call
	used  map[int64]tokens // tokens per hour bucket (unix hour → tokens)
}

// tokens is what a stretch of a transcript cost in tokens.
type tokens struct {
	In  int `json:"in"`  // read by the model: new input and cache reads/writes
	Out int `json:"out"` // written by the model
}

type linkMsg struct {
	fromPID int
	at      time.Time
}

var (
	linkCacheMu gosync.Mutex
	linkCache   = map[string]*transcriptScan{}

	// The receiver's transcript holds every delivered message, at the top
	// level of a prompt or inside a mid-turn reminder, with JSON escaping.
	crossRe   = regexp.MustCompile(`cross-session-message from=\\?"uds:[^"\\]*/(\d+)\.sock\\?"`)
	stampRe   = regexp.MustCompile(`"timestamp":"([^"]+)"`)
	handoffRe = regexp.MustCompile(`przeczytaj [^"\\]*handoffs/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.md`)

	modelRe   = regexp.MustCompile(`"model":"(claude-[a-z0-9-]+|[a-z]+)"`)
	toolUseRe = regexp.MustCompile(`"type":"tool_use","id":"[^"]*","name":"([A-Za-z0-9_]+)"`)
	usageRe   = regexp.MustCompile(`"usage":\{`)
	tokRe     = map[string]*regexp.Regexp{
		"in":    regexp.MustCompile(`"input_tokens":(\d+)`),
		"write": regexp.MustCompile(`"cache_creation_input_tokens":(\d+)`),
		"read":  regexp.MustCompile(`"cache_read_input_tokens":(\d+)`),
		"out":   regexp.MustCompile(`"output_tokens":(\d+)`),
	}
)

// scanTranscript brings the cached scan of path up to date.
func scanTranscript(path string) *transcriptScan {
	linkCacheMu.Lock()
	sc := linkCache[path]
	if sc == nil {
		sc = &transcriptScan{seen: map[string]bool{}}
		linkCache[path] = sc
	}
	linkCacheMu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return sc
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sc
	}
	if info.Size() < sc.offset { // rewritten: start over
		*sc = transcriptScan{seen: map[string]bool{}}
	}
	if info.Size() == sc.offset {
		return sc
	}
	if _, err := f.Seek(sc.offset, io.SeekStart); err != nil {
		return sc
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	pos := sc.offset
	for {
		line, err := rd.ReadString('\n')
		if err != nil { // a partial last line is read again next time
			break
		}
		pos += int64(len(line))
		scanLine(sc, line)
	}
	sc.offset = pos
	return sc
}

func scanLine(sc *transcriptScan, line string) {
	// Request blobs repeat the whole conversation; only real entries count.
	if strings.Contains(line[:min(len(line), 120)], `"api-request-blob"`) {
		return
	}
	if strings.Contains(line, `"type":"user"`) {
		sc.users++
		// Resumed from a handoff: it is the first thing typed after /clear,
		// so only the opening entries are looked at (later ones may quote
		// handoff paths of other sessions).
		if sc.parent == "" && sc.users <= 6 {
			if m := handoffRe.FindStringSubmatch(line); m != nil {
				sc.parent = m[1]
			}
		}
	}
	if strings.Contains(line, `"type":"assistant"`) {
		scanAssistant(sc, line)
	}
	if !strings.Contains(line, "cross-session-message from=") {
		return
	}
	var at time.Time
	if m := stampRe.FindStringSubmatch(line); m != nil {
		at, _ = time.Parse(time.RFC3339Nano, m[1])
	}
	for _, m := range crossRe.FindAllStringSubmatchIndex(line, -1) {
		pid, _ := strconv.Atoi(line[m[2]:m[3]])
		// The same message is echoed in later entries (tool results,
		// summaries): its first words identify it.
		key := line[m[0]:min(len(line), m[1]+160)]
		if pid == 0 || sc.seen[key] {
			continue
		}
		sc.seen[key] = true
		sc.msgs = append(sc.msgs, linkMsg{fromPID: pid, at: at})
	}
}

// scanAssistant reads what an assistant entry says about the model, the
// tool it called and the tokens it cost. Subagents (sidechains) have their
// own context, so only the main thread sets model and ctx; their tokens
// still count.
func scanAssistant(sc *transcriptScan, line string) {
	side := strings.Contains(line, `"isSidechain":true`)
	if !side {
		if m := modelRe.FindStringSubmatch(line); m != nil && m[1] != "synthetic" {
			sc.model = m[1]
		}
		if m := toolUseRe.FindStringSubmatch(line); m != nil {
			sc.tool = m[1]
		}
	}
	loc := usageRe.FindStringIndex(line)
	if loc == nil {
		return
	}
	blob := line[loc[1]:min(len(line), loc[1]+400)]
	n := func(k string) int {
		if m := tokRe[k].FindStringSubmatch(blob); m != nil {
			v, _ := strconv.Atoi(m[1])
			return v
		}
		return 0
	}
	u := tokens{In: n("in") + n("write") + n("read"), Out: n("out")}
	if u.In == 0 && u.Out == 0 {
		return
	}
	if !side {
		sc.ctx = u.In + u.Out
	}
	var at time.Time
	if m := stampRe.FindStringSubmatch(line); m != nil {
		at, _ = time.Parse(time.RFC3339Nano, m[1])
	}
	if sc.used == nil {
		sc.used = map[int64]tokens{}
	}
	h := at.Unix() / 3600
	b := sc.used[h]
	b.In += u.In
	b.Out += u.Out
	sc.used[h] = b
}

// usedSince sums the token buckets from since on (whole hours, so the
// first one may start a little earlier).
func (sc *transcriptScan) usedSince(since time.Time) tokens {
	var u tokens
	from := since.Unix() / 3600
	for h, b := range sc.used {
		if h >= from {
			u.In += b.In
			u.Out += b.Out
		}
	}
	return u
}

// boardStat is what the 8-bit board shows beside a character.
type boardStat struct {
	Model string `json:"model,omitempty"`
	Ctx   int    `json:"ctx,omitempty"`  // context of the last turn, tokens
	Tool  string `json:"tool,omitempty"` // last tool called
	Used  tokens `json:"used"`           // tokens in the window
}

// links counts messages between live sessions over the last ?hours (24).
func (s *webServer) links(w http.ResponseWriter, r *http.Request) {
	hours, err := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
	if err != nil || hours <= 0 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
	all, err := s.st.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Scans are cached and updated in place; two boards asking at once
	// would write the same maps (fatal "concurrent map writes").
	linksMu.Lock()
	edges := linkEdges(all, since, scanTranscript)
	stats := boardStats(all, since, scanTranscript)
	linksMu.Unlock()
	writeJSON(w, map[string]any{"hours": hours, "edges": edges, "stats": stats})
}

var linksMu gosync.Mutex

// linkEdges resolves every received message to a pair of live sessions:
// the sender by its process id (it survives /clear), the receiver by
// following handoffs from an old transcript to the session that took over.
func linkEdges(all []store.Session, since time.Time, scan func(string) *transcriptScan) []boardEdge {
	byPID := map[int]string{}
	live := map[string]bool{}
	for _, x := range all {
		if x.Live() && x.PID > 0 {
			byPID[x.PID] = x.ID
			live[x.ID] = true
		}
	}
	scans := map[string]*transcriptScan{}
	child := map[string]string{} // resumed-from → resumed-as
	for _, x := range all {
		if x.TranscriptPath == "" || x.UpdatedAt.Before(since) {
			continue
		}
		sc := scan(x.TranscriptPath)
		scans[x.ID] = sc
		if sc.parent != "" && sc.parent != x.ID {
			child[sc.parent] = x.ID
		}
	}
	resolve := func(id string) string {
		for i := 0; i < 64 && !live[id]; i++ {
			next, ok := child[id]
			if !ok {
				return ""
			}
			id = next
		}
		if live[id] {
			return id
		}
		return ""
	}

	type key struct{ from, to string }
	counts := map[key]*boardEdge{}
	for id, sc := range scans {
		to := resolve(id)
		if to == "" {
			continue
		}
		for _, m := range sc.msgs {
			from := byPID[m.fromPID]
			if from == "" || from == to || (!m.at.IsZero() && m.at.Before(since)) {
				continue
			}
			k := key{from, to}
			e := counts[k]
			if e == nil {
				e = &boardEdge{From: from, To: to}
				counts[k] = e
			}
			e.Count++
			if t := m.at.UTC().Format(time.RFC3339); t > e.Last {
				e.Last = t
			}
		}
	}
	out := make([]boardEdge, 0, len(counts))
	for _, e := range counts {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].From+out[i].To < out[j].From+out[j].To
	})
	return out
}

// boardStats reads model, context and tokens of every live session. The
// scans are cached, so after linkEdges this is a map lookup per session.
func boardStats(all []store.Session, since time.Time, scan func(string) *transcriptScan) map[string]boardStat {
	out := map[string]boardStat{}
	for _, x := range all {
		if !x.Live() || x.TranscriptPath == "" {
			continue
		}
		sc := scan(x.TranscriptPath)
		if sc.model == "" && sc.ctx == 0 && sc.tool == "" {
			continue
		}
		out[x.ID] = boardStat{Model: sc.model, Ctx: sc.ctx, Tool: sc.tool, Used: sc.usedSince(since)}
	}
	return out
}

// ---- speech bubbles ----
// What a character says comes from text harness already has; it is cut to
// one short, plain sentence here so the page never shows markdown, paths
// or a wall of text. No model is asked.

var (
	fenceRe    = regexp.MustCompile("(?s)```.*?```|```.*$")
	mdLinkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	urlRe      = regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://\S+`)
	pathRe     = regexp.MustCompile("(?:`[^`]*`)|(?:(?:~|\\.{1,2})?/[\\w.@~-]+(?:/[\\w.@~-]*)+/?)|(?:\\b[\\w.-]+/[\\w./-]+)")
	markRe     = regexp.MustCompile("[*_`>|]+|(?m)^#+\\s*")
	parensRe   = regexp.MustCompile(`\(\s*\)`)
	beforeRe   = regexp.MustCompile(`\s+([,.;:!?])`)
	bulletRe   = regexp.MustCompile(`(?m)^\s*(?:[-*+•]|\d+[.)])\s+`)
	spaceRe    = regexp.MustCompile(`\s+`)
	sentenceRe = regexp.MustCompile(`[.!?…](?:\s|$)`)
	prRe       = regexp.MustCompile(`(?:PR\s*)?#(\d{2,6})\b`)
)

// sayMax is the longest line a bubble shows.
const sayMax = 80

// sayLine turns a message into one bubble: the first sentence, without
// markdown, code, paths and links, at most sayMax characters.
func sayLine(text string) string {
	t := fenceRe.ReplaceAllString(text, " ")
	t = mdLinkRe.ReplaceAllString(t, "$1")
	t = urlRe.ReplaceAllString(t, " ")
	t = bulletRe.ReplaceAllString(t, " ")
	t = pathRe.ReplaceAllString(t, " ")
	t = markRe.ReplaceAllString(t, " ")
	t = parensRe.ReplaceAllString(t, " ")
	t = beforeRe.ReplaceAllString(t, "$1")
	t = strings.TrimSpace(spaceRe.ReplaceAllString(t, " "))
	t = strings.TrimLeft(t, "?> ")
	// The first sentence. A stop after a short word or a number ("np.",
	// "1.5 s.") ends it only when a capital letter follows.
	for _, m := range sentenceRe.FindAllStringIndex(t, -1) {
		head := t[:m[0]]
		if i := strings.LastIndexByte(head, ' '); i >= 0 {
			head = head[i+1:]
		}
		if len([]rune(head)) <= 3 || isDigits(head) {
			rest := strings.TrimSpace(t[m[1]:])
			if rest != "" && !startsUpper(rest) {
				continue
			}
		}
		t = strings.TrimSpace(t[:m[0]+1])
		break
	}
	t = strings.TrimSpace(strings.TrimRight(t, ",:;-–"))
	r := []rune(t)
	if len(r) > sayMax {
		cut := sayMax - 1
		for ; cut > sayMax/2 && r[cut] != ' '; cut-- {
		}
		t = strings.TrimRight(string(r[:cut]), " ,:;-–.") + "…"
	}
	return t
}

func startsUpper(s string) bool {
	r := []rune(s)[0]
	return unicode.IsUpper(r) || unicode.IsDigit(r)
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// sayFor is the bubble of a session: the question when it waits, the
// prompt it works on, otherwise its last message.
func sayFor(s store.Session) string {
	switch {
	case s.Status == store.Waiting && s.Detail != "":
		return sayLine(s.Detail)
	case s.Status == store.Working && s.LastPrompt != "":
		return sayLine(s.LastPrompt)
	}
	return sayLine(s.LastMessage)
}

// prNumber finds the pull request a session is on: in its name first, then
// in its last message.
func prNumber(s store.Session) int {
	for _, t := range []string{s.Name, s.LastMessage} {
		if m := prRe.FindStringSubmatch(t); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// ---- live keyboard ----

// liveKeyRe is what live typing may press besides literal text: the keys of
// an ordinary keyboard and Ctrl/Alt with a letter. Mouse and tmux commands
// stay out.
var liveKeyRe = regexp.MustCompile(`^(Enter|Escape|BSpace|Tab|BTab|Up|Down|Left|Right|Home|End|PageUp|PageDown|DC|Space|M-Enter|M-BSpace|M-Left|M-Right|C-[a-z]|M-[a-z])$`)

// maxLiveEvents bounds one batch: the browser sends what was typed since the
// previous request returned, a paste is one text event.
const maxLiveEvents = 256

// liveKeys types a batch of key events into the session's pane, in order.
func (s *webServer) liveKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []tmux.KeyEvent `json:"events"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Events) == 0 || len(body.Events) > maxLiveEvents {
		http.Error(w, "1 to 256 events", http.StatusBadRequest)
		return
	}
	for _, ev := range body.Events {
		if (ev.Text == "") == (ev.Key == "") || (ev.Key != "" && !liveKeyRe.MatchString(ev.Key)) {
			http.Error(w, "event not allowed: "+ev.Key, http.StatusBadRequest)
			return
		}
	}
	se, err := s.st.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if se.TmuxPane == "" || !paneExists(se.TmuxPane) {
		http.Error(w, "session is not running in harness", http.StatusConflict)
		return
	}
	s.lastInput.Store(se.TmuxPane, time.Now())
	if err := tmux.SendKeys(se.TmuxPane, body.Events); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

// sysSampler feeds the board's system panel; it samples only while a board
// keeps asking.
var sysSampler sysstat.Sampler

func (s *webServer) system(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"every": sysstat.Every.Milliseconds(), "samples": sysSampler.History()})
}
