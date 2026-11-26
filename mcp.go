package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	gosync "sync"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
)

// cmdMCP is a stdio MCP server that lets an agent call the user:
//
//	claude mcp add --scope user harness -- harness mcp
//
// huddle_call rings the phone and blocks until the user asks the agent
// something (answered with huddle_answer, which blocks again) or the call
// ends. The call itself runs in `harness serve`; this process only talks
// to it over localhost.
func cmdMCP(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := webToken(cfg)
	if err != nil {
		return err
	}
	base := strings.TrimRight(os.Getenv("HARNESS_SERVE_URL"), "/")
	if base == "" {
		base = "http://127.0.0.1:7777"
	}
	srv := &mcpServer{cfg: cfg, base: base, token: token, out: bufio.NewWriter(os.Stdout),
		http: &http.Client{Timeout: 90 * time.Second}}
	return srv.run(os.Stdin)
}

type mcpServer struct {
	cfg   *config.Config
	base  string
	token string
	http  *http.Client

	mu  gosync.Mutex
	out *bufio.Writer
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *mcpServer) send(m rpcMsg) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *mcpServer) run(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var wg gosync.WaitGroup
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method == "" {
			continue
		}
		if len(m.ID) == 0 { // a notification
			continue
		}
		wg.Add(1)
		go func(m rpcMsg) { // a call can last minutes: keep reading meanwhile
			defer wg.Done()
			res, err := s.handle(m)
			if err != nil {
				s.send(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
				return
			}
			s.send(rpcMsg{ID: m.ID, Result: res})
		}(m)
	}
	wg.Wait()
	return sc.Err()
}

var mcpTools = []map[string]any{
	{
		"name": "huddle_call",
		"description": "Call the user on their phone for a voice conversation (like a Slack huddle) about the work in this session. " +
			"A voice assistant talks to the user using your brief. Use it for brainstorming or for several related decisions that are easier to talk through than to type; " +
			"not for a single yes/no question (use telegram-ask) and never for routine updates. The call blocks: it returns either a question from the user " +
			"that you must answer with huddle_answer (quickly and briefly: it is read aloud) or, when the call ends, the transcript and the decisions. " +
			"Refused during quiet hours and above the hourly limit.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{"type": "string", "description": "What the call is about, one short line (shown on the phone as it rings)."},
				"brief": map[string]any{"type": "string", "description": "Everything the voice needs: context of the task, what is done, the options you see with your recommendation and why, constraints. Plain prose, Polish or English."},
				"questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "The questions to settle, in order."},
			},
			"required": []string{"topic", "brief"},
		},
	},
	{
		"name":        "huddle_answer",
		"description": "Answer a question the user asked during a huddle (huddle_call returned it). Then blocks again like huddle_call: the next question or the end of the call.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"huddle_id": map[string]any{"type": "string"},
				"answer":    map[string]any{"type": "string", "description": "A short spoken-style answer: a few sentences, no code, no tables."},
			},
			"required": []string{"huddle_id", "answer"},
		},
	},
	{
		"name":        "huddle_end",
		"description": "Hang up a huddle (e.g. you got what you needed). Returns the transcript and the decisions.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"huddle_id": map[string]any{"type": "string"}},
			"required":   []string{"huddle_id"},
		},
	},
}

func (s *mcpServer) handle(m rpcMsg) (any, error) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "harness", "version": "1"},
			"instructions":    "huddle_call rings the user's phone for a voice conversation. Use it sparingly: brainstorming or several decisions at once.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, err
		}
		text, err := s.callTool(p.Name, p.Arguments, p.Meta.ProgressToken)
		if err != nil {
			return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}, nil
	}
	return nil, fmt.Errorf("method not found: %s", m.Method)
}

func (s *mcpServer) callTool(name string, raw json.RawMessage, progress any) (string, error) {
	var a struct {
		Topic     string   `json:"topic"`
		Brief     string   `json:"brief"`
		Questions []string `json:"questions"`
		HuddleID  string   `json:"huddle_id"`
		Answer    string   `json:"answer"`
	}
	json.Unmarshal(raw, &a)
	switch name {
	case "huddle_call":
		req := huddleRequest{Topic: a.Topic, Brief: a.Brief, Questions: a.Questions, Origin: originAgent}
		if req.Brief == "" {
			req.Brief = a.Topic
		}
		if st, err := store.Open(s.cfg.StatePath()); err == nil {
			if se, ok := callingSession(st, ""); ok {
				req.SessionID = se.ID
			}
			st.Close()
		}
		var h struct {
			ID string `json:"id"`
		}
		if err := s.api("POST", "/api/huddle", req, &h); err != nil {
			return "", err
		}
		return s.wait(h.ID, progress)
	case "huddle_answer":
		if err := s.api("POST", "/api/huddle/"+a.HuddleID+"/answer", map[string]string{"text": a.Answer}, nil); err != nil {
			return "", err
		}
		return s.wait(a.HuddleID, progress)
	case "huddle_end":
		if err := s.api("POST", "/api/huddle/"+a.HuddleID+"/cancel", map[string]string{}, nil); err != nil {
			return "", err
		}
		return s.wait(a.HuddleID, progress)
	}
	return "", fmt.Errorf("unknown tool %s", name)
}

// wait long-polls the huddle until there is something for the agent.
func (s *mcpServer) wait(id string, progress any) (string, error) {
	for n := 1; ; n++ {
		var ev agentEvent
		if err := s.api("GET", "/api/huddle/"+id+"/next?wait=50", nil, &ev); err != nil {
			return "", err
		}
		switch ev.Type {
		case "question":
			return fmt.Sprintf("The user asks (huddle %s, call still live, they are waiting):\n\n%s\n\nAnswer with huddle_answer(huddle_id=%q, answer=…) right away: a few spoken sentences, no code.", id, ev.Question, id), nil
		case "ended":
			return renderEnded(id, ev), nil
		}
		if progress != nil { // keeps the client from timing the call out
			s.send(rpcMsg{Method: "notifications/progress", Params: mustJSON(map[string]any{
				"progressToken": progress, "progress": n, "message": "huddle " + ev.Status})})
		}
	}
}

func renderEnded(id string, ev agentEvent) string {
	var b strings.Builder
	switch ev.Status {
	case hMissed, hDeclined:
		fmt.Fprintf(&b, "The user did not take the call (%s). Do not call again right away: carry on with a sensible assumption and note it, or ask with telegram-ask.\n", ev.Status)
		return b.String()
	case hCancelled:
		fmt.Fprintf(&b, "The call did not happen (%s).\n", ev.Reason)
		return b.String()
	}
	fmt.Fprintf(&b, "Huddle %s ended (%s).\n\n", id, ev.Reason)
	if len(ev.Decisions) > 0 {
		b.WriteString("Decisions:\n")
		for _, d := range ev.Decisions {
			b.WriteString("- " + d + "\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString("No decisions were recorded: read the transcript.\n\n")
	}
	if ev.Transcript != "" {
		b.WriteString("Transcript (speech-to-text, may contain recognition errors):\n" + ev.Transcript)
	}
	if ev.TranscriptPath != "" {
		b.WriteString("\nSaved to " + ev.TranscriptPath + "\n")
	}
	return b.String()
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *mcpServer) api(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("harness serve is not reachable at %s (is it running?): %w", s.base, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("harness serve: %s %s", res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
