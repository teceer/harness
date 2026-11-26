package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/coder/websocket"
	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
	"github.com/teceer/harness/internal/transcript"
)

// A huddle is a voice call about the work in a session, like a Slack huddle:
//
//   - an agent calls (MCP tool huddle_call → POST /api/huddle with a brief):
//     the phone rings through CallKit, the agent blocks on …/next and gets
//     the user's questions to answer and, at the end, the transcript and the
//     decisions;
//   - the user calls a session from the app (POST /api/huddle {sessionId}):
//     the brief is the session's recent conversation, questions go to the
//     session through tmux and the decisions are sent to it afterwards.
//
// The voice is a Gemini Live session bridged to the phone's WebSocket.

const (
	originAgent = "agent"
	originUser  = "user"

	hRinging   = "ringing"
	hLive      = "live"
	hEnded     = "ended"
	hDeclined  = "declined"
	hMissed    = "missed"
	hCancelled = "cancelled"
)

type huddleLine struct {
	Role string `json:"role"` // user | model | agent
	Text string `json:"text"`
}

// agentEvent is what the calling agent gets from …/next.
type agentEvent struct {
	Type           string   `json:"type"` // question | ended | pending
	Question       string   `json:"question,omitempty"`
	Status         string   `json:"status,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Decisions      []string `json:"decisions,omitempty"`
	Transcript     string   `json:"transcript,omitempty"`
	TranscriptPath string   `json:"transcriptPath,omitempty"`
}

type huddle struct {
	ID, Caller, Topic, Brief, Origin string
	Questions                        []string
	SessionID                        string
	Created                          time.Time

	mu        gosync.Mutex
	status    string
	reason    string
	lines     []huddleLine
	decisions []string
	events    []agentEvent
	final     *agentEvent
	sig       chan struct{} // closed and replaced when an event is queued
	answers   chan string   // the agent's answers, said by the voice
	phone     *websocket.Conn
	phoneGen  int
	live      *liveState
	stop      context.CancelFunc // ends the live bridge
	done      chan struct{}
}

func (h *huddle) view() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{"id": h.ID, "caller": h.Caller, "topic": h.Topic, "status": h.status,
		"origin": h.Origin, "sessionId": h.SessionID, "transcript": append([]huddleLine(nil), h.lines...)}
}

// addLine appends a transcript delta, merging it with the last line of the
// same speaker.
func (h *huddle) addLine(role, text string) {
	if text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.lines); n > 0 && h.lines[n-1].Role == role {
		h.lines[n-1].Text = joinDelta(h.lines[n-1].Text, text)
		return
	}
	h.lines = append(h.lines, huddleLine{role, strings.TrimLeft(text, " ")})
}

// joinDelta appends a transcription delta. Deltas inside a sentence carry
// their own spacing, but a new turn of the same speaker starts without one
// ("demo.Świetnie"), so a space goes after a sentence end.
func joinDelta(prev, delta string) string {
	if prev == "" {
		return strings.TrimLeft(delta, " ")
	}
	last := prev[len(prev)-1]
	if (last == '.' || last == '!' || last == '?') && !strings.HasPrefix(delta, " ") {
		return prev + " " + delta
	}
	return prev + delta
}

func (h *huddle) push(ev agentEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
	close(h.sig)
	h.sig = make(chan struct{})
}

type huddleManager struct {
	cfg  *config.Config
	st   *store.Store
	voip *voipTokens
	apns *apnsClient

	mu         gosync.Mutex
	all        map[string]*huddle
	active     *huddle
	agentCalls []time.Time

	// dial is the voice model (tests replace it).
	dial func(ctx context.Context, system string, tools []liveTool, handle string) (liveConn, error)
	// ringFn rings the phone (tests replace it).
	ringFn func(*huddle) error
}

func newHuddleManager(cfg *config.Config, st *store.Store) *huddleManager {
	m := &huddleManager{cfg: cfg, st: st, voip: newVoipTokens(cfg), all: map[string]*huddle{}}
	m.dial = func(ctx context.Context, system string, tools []liveTool, handle string) (liveConn, error) {
		c, err := dialLive(ctx, cfg.Huddle, system, tools, handle)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	m.ringFn = m.ring
	return m
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// inQuiet reports whether t falls in "HH:MM-HH:MM" (which may wrap midnight).
func inQuiet(spec string, t time.Time) bool {
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return false
	}
	parse := func(s string) (int, bool) {
		hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
		h, e1 := strconv.Atoi(hh)
		m, e2 := strconv.Atoi(mm)
		return h*60 + m, ok && e1 == nil && e2 == nil
	}
	from, ok1 := parse(a)
	to, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	if from <= to {
		return now >= from && now < to
	}
	return now >= from || now < to
}

type huddleRequest struct {
	SessionID string   `json:"sessionId"`
	Topic     string   `json:"topic"`
	Brief     string   `json:"brief"`
	Questions []string `json:"questions"`
	Origin    string   `json:"origin"`
}

// httpErr carries a status for the handler.
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

func (m *huddleManager) start(req huddleRequest) (*huddle, error) {
	origin := originUser
	if req.Origin == originAgent || strings.TrimSpace(req.Brief) != "" {
		origin = originAgent
	}
	h := &huddle{ID: newUUID(), Origin: origin, SessionID: req.SessionID, Created: time.Now(),
		Topic: trim(strings.TrimSpace(req.Topic), 120), Brief: trim(req.Brief, 20000),
		status: hRinging, sig: make(chan struct{}), answers: make(chan string, 8), done: make(chan struct{})}
	for _, q := range req.Questions {
		if q = strings.TrimSpace(q); q != "" {
			h.Questions = append(h.Questions, trim(q, 500))
		}
	}
	var se store.Session
	if req.SessionID != "" {
		var err error
		if se, err = m.st.Find(req.SessionID); err != nil {
			return nil, &httpErr{http.StatusNotFound, err.Error()}
		}
		h.SessionID = se.ID
		h.Caller = label(se)
	}
	if h.Caller == "" {
		h.Caller = "Agent"
	}
	if origin == originUser {
		if se.ID == "" {
			return nil, &httpErr{http.StatusBadRequest, "sessionId is required"}
		}
		if h.Topic == "" {
			h.Topic = "Rozmowa o " + h.Caller
		}
		h.Brief = sessionBrief(se)
	} else if h.Topic == "" {
		return nil, &httpErr{http.StatusBadRequest, "topic is required"}
	}

	m.mu.Lock()
	if a := m.active; a != nil {
		m.mu.Unlock()
		return nil, &httpErr{http.StatusConflict, fmt.Sprintf("another huddle is %s (%s)", a.statusNow(), a.Caller)}
	}
	if origin == originAgent {
		now := time.Now()
		if inQuiet(m.cfg.Huddle.QuietHours, now) {
			m.mu.Unlock()
			return nil, &httpErr{http.StatusTooManyRequests, "quiet hours (" + m.cfg.Huddle.QuietHours + "): do not call now, ask with telegram-ask instead"}
		}
		recent := m.agentCalls[:0]
		for _, t := range m.agentCalls {
			if now.Sub(t) < time.Hour {
				recent = append(recent, t)
			}
		}
		m.agentCalls = recent
		if len(recent) >= m.cfg.Huddle.MaxPerHour {
			m.mu.Unlock()
			return nil, &httpErr{http.StatusTooManyRequests, fmt.Sprintf("%d calls in the last hour already: collect the questions and call later, or use telegram-ask", len(recent))}
		}
		m.agentCalls = append(m.agentCalls, now)
	}
	m.active = h
	m.all[h.ID] = h
	m.mu.Unlock()

	ring := time.Duration(m.cfg.Huddle.RingSeconds) * time.Second
	if origin == originAgent {
		if err := m.ringFn(h); err != nil {
			m.finish(h, hCancelled, "ring failed")
			return nil, &httpErr{http.StatusServiceUnavailable, err.Error()}
		}
	} else {
		ring = 60 * time.Second // the phone connects right away; this only cleans up
	}
	time.AfterFunc(ring, func() {
		if h.statusNow() == hRinging {
			m.finish(h, hMissed, "no answer")
		}
	})
	log.Printf("huddle %s: %s call, %s: %s", short(h.ID), origin, h.Caller, h.Topic)
	return h, nil
}

func (h *huddle) statusNow() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (m *huddleManager) get(id string) *huddle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all[id]
}

// finish ends a huddle once: closes the phone and the voice, writes the
// transcript, hands the result to the agent or the decisions to the session.
func (m *huddleManager) finish(h *huddle, status, reason string) {
	h.mu.Lock()
	if h.final != nil {
		h.mu.Unlock()
		return
	}
	if status == hEnded && h.status == hRinging {
		status = hCancelled
	}
	h.status, h.reason = status, reason
	phone, stop := h.phone, h.stop
	h.final = &agentEvent{} // placeholder: finish runs once
	h.mu.Unlock()

	if phone != nil { // Close waits for the phone's handshake: not on this path
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			writeJSONWS(ctx, phone, map[string]string{"type": "ended", "reason": reason})
			phone.Close(websocket.StatusNormalClosure, reason)
		}()
	}
	if stop != nil {
		stop()
	}
	m.mu.Lock()
	if m.active == h {
		m.active = nil
	}
	if len(m.all) > 30 { // keep the recent ones for GET
		for id, x := range m.all {
			if x != h && time.Since(x.Created) > 24*time.Hour {
				delete(m.all, id)
			}
		}
	}
	m.mu.Unlock()

	h.mu.Lock()
	lines := append([]huddleLine(nil), h.lines...)
	decisions := append([]string(nil), h.decisions...)
	h.mu.Unlock()
	path := ""
	if len(lines) > 0 {
		path = m.writeTranscript(h, status, lines, decisions)
	}
	ev := agentEvent{Type: "ended", Status: status, Reason: reason, Decisions: decisions,
		Transcript: renderLines(lines), TranscriptPath: path}
	h.mu.Lock()
	h.final = &ev
	h.events = append(h.events, ev)
	close(h.sig)
	h.sig = make(chan struct{})
	h.mu.Unlock()
	close(h.done)
	log.Printf("huddle %s: %s (%s), %d lines, %d decisions", short(h.ID), status, reason, len(lines), len(decisions))

	if h.Origin == originUser && len(decisions) > 0 {
		m.tellSession(h, "[huddle] Ustalenia z rozmowy głosowej z użytkownikiem: "+numbered(decisions)+
			". Pełny transkrypt: "+path+". Kontynuuj zgodnie z ustaleniami.")
	}
}

func numbered(items []string) string {
	var b strings.Builder
	for i, d := range items {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, oneLine(d))
	}
	return b.String()
}

// oneLine keeps text typed into a session on one line: a newline would
// submit the prompt half-written.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func renderLines(lines []huddleLine) string {
	var b strings.Builder
	for _, l := range lines {
		who := map[string]string{"user": "Użytkownik", "model": "Głos agenta", "agent": "Agent"}[l.Role]
		fmt.Fprintf(&b, "%s: %s\n", who, strings.TrimSpace(l.Text))
	}
	return b.String()
}

func (m *huddleManager) writeTranscript(h *huddle, status string, lines []huddleLine, decisions []string) string {
	dir := filepath.Join(m.cfg.Home, "huddles")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, h.Created.Format("2006-01-02-1504")+"-"+short(h.ID)+".md")
	var b strings.Builder
	fmt.Fprintf(&b, "# Huddle: %s\n\n- Sesja: %s\n- Kto dzwonił: %s\n- Status: %s\n- Start: %s\n\n",
		h.Topic, h.Caller, map[string]string{originAgent: "agent", originUser: "użytkownik"}[h.Origin], status, h.Created.Format(time.RFC3339))
	if len(decisions) > 0 {
		b.WriteString("## Ustalenia\n\n")
		for _, d := range decisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Transkrypt\n\n")
	for _, l := range strings.Split(strings.TrimSpace(renderLines(lines)), "\n") {
		fmt.Fprintf(&b, "%s\n\n", l)
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	return path
}

// tellSession types a one-line message into the session's prompt.
func (m *huddleManager) tellSession(h *huddle, text string) error {
	se, err := m.st.Find(h.SessionID)
	if err != nil {
		return err
	}
	if se.TmuxPane == "" || !paneExists(se.TmuxPane) {
		return errors.New("session is not running in harness")
	}
	return tmux.SendText(se.TmuxPane, oneLine(text))
}

// sessionBrief is what the voice knows when the user calls a session: its
// place and the end of its conversation.
func sessionBrief(se store.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sesja: %s\nKatalog: %s\nStatus: %s %s\n", label(se), se.Cwd, se.Status, se.Detail)
	msgs, _ := transcript.Tail(se.TranscriptPath, 14)
	if len(msgs) > 0 {
		b.WriteString("\nOstatnie wiadomości w sesji (najnowsze na końcu):\n")
	}
	budget := 9000
	var parts []string
	for i := len(msgs) - 1; i >= 0 && budget > 0; i-- {
		who := "Użytkownik"
		if msgs[i].Role == "assistant" {
			who = "Agent"
		}
		t := trim(strings.TrimSpace(msgs[i].Text), 2000)
		budget -= len(t)
		parts = append([]string{who + ": " + t}, parts...)
	}
	b.WriteString(strings.Join(parts, "\n\n"))
	return b.String()
}

// ---- HTTP ----

func (s *webServer) huddleRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/push/voip/register", s.auth(s.voipRegister))
	mux.Handle("POST /api/push/voip/unregister", s.auth(s.voipUnregister))
	mux.Handle("POST /api/huddle", s.auth(s.huddleStart))
	mux.Handle("GET /api/huddle", s.auth(s.huddleActive))
	mux.Handle("GET /api/huddle/{id}", s.auth(s.huddleGet))
	mux.Handle("POST /api/huddle/{id}/decline", s.auth(s.huddleDecline))
	mux.Handle("POST /api/huddle/{id}/cancel", s.auth(s.huddleCancel))
	mux.Handle("GET /api/huddle/{id}/next", s.auth(s.huddleNext))
	mux.Handle("POST /api/huddle/{id}/answer", s.auth(s.huddleAnswer))
	mux.Handle("GET /api/huddle/{id}/live", s.auth(s.huddleLive))
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *webServer) huddleStart(w http.ResponseWriter, r *http.Request) {
	var req huddleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.huddles.start(req)
	if err != nil {
		var he *httpErr
		if errors.As(err, &he) {
			jsonError(w, he.code, he.msg)
		} else {
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.logAction(r, "huddle %s (%s)", short(h.ID), h.Origin)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(h.view())
}

func (s *webServer) huddleActive(w http.ResponseWriter, r *http.Request) {
	s.huddles.mu.Lock()
	a := s.huddles.active
	s.huddles.mu.Unlock()
	if a == nil {
		writeJSON(w, map[string]any{"active": nil})
		return
	}
	writeJSON(w, map[string]any{"active": a.view()})
}

func (s *webServer) huddleOr404(w http.ResponseWriter, r *http.Request) *huddle {
	h := s.huddles.get(r.PathValue("id"))
	if h == nil {
		jsonError(w, http.StatusNotFound, "no such huddle")
	}
	return h
}

func (s *webServer) huddleGet(w http.ResponseWriter, r *http.Request) {
	if h := s.huddleOr404(w, r); h != nil {
		writeJSON(w, h.view())
	}
}

func (s *webServer) huddleDecline(w http.ResponseWriter, r *http.Request) {
	if h := s.huddleOr404(w, r); h != nil {
		if h.statusNow() == hRinging {
			s.huddles.finish(h, hDeclined, "declined")
		}
		writeJSON(w, map[string]string{"ok": "1"})
	}
}

func (s *webServer) huddleCancel(w http.ResponseWriter, r *http.Request) {
	if h := s.huddleOr404(w, r); h != nil {
		s.huddles.finish(h, hEnded, "agent")
		writeJSON(w, map[string]string{"ok": "1"})
	}
}

// huddleNext long-polls the next event for the calling agent.
func (s *webServer) huddleNext(w http.ResponseWriter, r *http.Request) {
	h := s.huddleOr404(w, r)
	if h == nil {
		return
	}
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > 120 {
		wait = 50
	}
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	for {
		h.mu.Lock()
		if len(h.events) > 0 {
			ev := h.events[0]
			h.events = h.events[1:]
			h.mu.Unlock()
			writeJSON(w, ev)
			return
		}
		if h.final != nil && h.final.Type == "ended" {
			ev := *h.final
			h.mu.Unlock()
			writeJSON(w, ev)
			return
		}
		sig, status := h.sig, h.status
		h.mu.Unlock()
		select {
		case <-sig:
		case <-timer.C:
			writeJSON(w, agentEvent{Type: "pending", Status: status})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *webServer) huddleAnswer(w http.ResponseWriter, r *http.Request) {
	h := s.huddleOr404(w, r)
	if h == nil {
		return
	}
	var body struct{ Text string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		jsonError(w, http.StatusBadRequest, "text is required")
		return
	}
	select {
	case <-h.done:
		jsonError(w, http.StatusGone, "the huddle has ended")
		return
	default:
	}
	select {
	case h.answers <- body.Text:
		writeJSON(w, map[string]string{"ok": "1"})
	default:
		jsonError(w, http.StatusTooManyRequests, "answers queue is full")
	}
}

// huddleLive is the phone's audio connection.
func (s *webServer) huddleLive(w http.ResponseWriter, r *http.Request) {
	h := s.huddleOr404(w, r)
	if h == nil {
		return
	}
	if st := h.statusNow(); st != hRinging && st != hLive {
		jsonError(w, http.StatusGone, "the huddle is "+st)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // the bearer token is the check
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	s.logAction(r, "huddle %s: phone connected", short(h.ID))
	s.huddles.serveLive(h, c)
}
