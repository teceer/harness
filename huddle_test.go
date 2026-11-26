package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/teceer/harness/internal/config"
)

// fakeLive stands in for Gemini Live: the test feeds server messages and
// reads what the bridge wrote.
type fakeLive struct {
	in     chan []byte
	mu     gosync.Mutex
	writes []string
	closed chan struct{}
	once   gosync.Once
}

func newFakeLive() *fakeLive {
	return &fakeLive{in: make(chan []byte, 32), closed: make(chan struct{})}
}

func (f *fakeLive) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case b := <-f.in:
		return websocket.MessageText, b, nil
	case <-f.closed:
		return 0, nil, errors.New("closed")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (f *fakeLive) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	f.writes = append(f.writes, string(p))
	f.mu.Unlock()
	return nil
}

func (f *fakeLive) CloseNow() error { f.once.Do(func() { close(f.closed) }); return nil }

func (f *fakeLive) wrote(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func huddleServer(t *testing.T) (*webServer, *httptest.Server, *fakeLive, *[]string) {
	t.Helper()
	srv, _ := testServer(t)
	srv.huddles = newHuddleManager(srv.cfg, srv.st)
	live := newFakeLive()
	srv.huddles.dial = func(ctx context.Context, system string, tools []liveTool, handle string) (liveConn, error) {
		return live, nil
	}
	var rang []string
	srv.huddles.ringFn = func(h *huddle) error { rang = append(rang, h.ID); return nil }
	mux := http.NewServeMux()
	srv.routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts, live, &rang
}

func apiJSON(t *testing.T, ts *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer secret-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func dialPhone(t *testing.T, ts *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/huddle/" + id + "/live"
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// phoneRead returns the next text message of the given type.
func phoneRead(t *testing.T, c *websocket.Conn, typ string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		mt, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("phone waiting for %s: %v", typ, err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var m map[string]string
		json.Unmarshal(b, &m)
		if m["type"] == typ {
			return m
		}
	}
}

func TestHuddleAgentCall(t *testing.T) {
	_, ts, live, rang := huddleServer(t)

	var h map[string]any
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{SessionID: "sess-1", Topic: "Bramki płatności",
		Brief: "Wybieramy bramkę.", Questions: []string{"P24 czy Stripe?"}}, &h); code != http.StatusCreated {
		t.Fatalf("start = %d %v", code, h)
	}
	id := h["id"].(string)
	if len(*rang) != 1 || h["status"] != hRinging || h["origin"] != originAgent {
		t.Fatalf("not ringing: %v rang=%v", h, *rang)
	}
	// One call at a time.
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "x", Brief: "y"}, nil); code != http.StatusConflict {
		t.Fatalf("second call = %d, want 409", code)
	}

	phone := dialPhone(t, ts, id)
	defer phone.CloseNow()
	phoneRead(t, phone, "state") // connecting
	if st := phoneRead(t, phone, "state"); st["state"] != "live" {
		t.Fatalf("state = %v", st)
	}
	waitFor(t, "kickoff", func() bool { return live.wrote("[SYSTEM]") })

	// Mic audio goes to the voice.
	phone.Write(context.Background(), websocket.MessageBinary, []byte{1, 2, 3, 4})
	waitFor(t, "audio forwarded", func() bool { return live.wrote("audio/pcm;rate=16000") })

	// The voice speaks and transcribes; the phone gets both.
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 4800))
	live.in <- []byte(`{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"` + pcm + `"}}]},"outputTranscription":{"text":"Cześć"}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		mt, b, err := phone.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if mt == websocket.MessageBinary {
			if len(b) != 4800 {
				t.Fatalf("audio %d bytes", len(b))
			}
			break
		}
	}
	if tr := phoneRead(t, phone, "transcript"); tr["role"] != "model" || tr["text"] != "Cześć" {
		t.Fatalf("transcript = %v", tr)
	}

	// The voice asks the agent; the agent's long-poll gets the question.
	live.in <- []byte(`{"toolCall":{"functionCalls":[{"id":"c1","name":"ask_agent","args":{"question":"Jaki branch?"}}]}}`)
	var ev agentEvent
	apiJSON(t, ts, "GET", "/api/huddle/"+id+"/next?wait=5", nil, &ev)
	if ev.Type != "question" || ev.Question != "Jaki branch?" {
		t.Fatalf("next = %+v", ev)
	}
	waitFor(t, "tool response", func() bool { return live.wrote(`"toolResponse"`) })
	if code := apiJSON(t, ts, "POST", "/api/huddle/"+id+"/answer", map[string]string{"text": "feature/huddle"}, nil); code != 200 {
		t.Fatalf("answer = %d", code)
	}
	waitFor(t, "answer said", func() bool { return live.wrote("[AGENT]") && live.wrote("feature/huddle") })

	// A decision, then the user hangs up.
	live.in <- []byte(`{"toolCall":{"functionCalls":[{"id":"c2","name":"save_decision","args":{"decision":"Stripe."}}]}}`)
	live.in <- []byte(`{"serverContent":{"inputTranscription":{"text":"Bierzemy Stripe"}}}`)
	waitFor(t, "decision", func() bool { return live.wrote(`"c2"`) })
	time.Sleep(50 * time.Millisecond)
	phone.Write(context.Background(), websocket.MessageText, []byte(`{"type":"hangup"}`))

	ev = agentEvent{}
	apiJSON(t, ts, "GET", "/api/huddle/"+id+"/next?wait=5", nil, &ev)
	if ev.Type != "ended" || ev.Status != hEnded || len(ev.Decisions) != 1 || ev.Decisions[0] != "Stripe." {
		t.Fatalf("ended = %+v", ev)
	}
	if !strings.Contains(ev.Transcript, "Użytkownik: Bierzemy Stripe") || ev.TranscriptPath == "" {
		t.Fatalf("transcript = %q path=%q", ev.Transcript, ev.TranscriptPath)
	}
	if b, err := os.ReadFile(ev.TranscriptPath); err != nil || !strings.Contains(string(b), "## Ustalenia") {
		t.Fatalf("transcript file: %v %s", err, b)
	}
	// Ended stays ended, and the line is free again.
	apiJSON(t, ts, "GET", "/api/huddle/"+id+"/next?wait=1", nil, &ev)
	if ev.Type != "ended" {
		t.Fatalf("second next = %+v", ev)
	}
	var active map[string]any
	apiJSON(t, ts, "GET", "/api/huddle", nil, &active)
	if active["active"] != nil {
		t.Fatalf("active = %v", active)
	}
}

func TestHuddleDeclineAndMissed(t *testing.T) {
	srv, ts, _, _ := huddleServer(t)
	srv.cfg.Huddle.RingSeconds = 1
	var h map[string]any
	apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "a", Brief: "b"}, &h)
	apiJSON(t, ts, "POST", "/api/huddle/"+h["id"].(string)+"/decline", nil, nil)
	var ev agentEvent
	apiJSON(t, ts, "GET", "/api/huddle/"+h["id"].(string)+"/next?wait=2", nil, &ev)
	if ev.Status != hDeclined {
		t.Fatalf("declined = %+v", ev)
	}

	apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "a", Brief: "b"}, &h)
	apiJSON(t, ts, "GET", "/api/huddle/"+h["id"].(string)+"/next?wait=5", nil, &ev)
	if ev.Status != hMissed {
		t.Fatalf("missed = %+v", ev)
	}
	if !strings.Contains(renderEnded("x", ev), "did not take the call") {
		t.Fatal(renderEnded("x", ev))
	}
}

func TestHuddlePolicy(t *testing.T) {
	srv, ts, _, rang := huddleServer(t)
	srv.cfg.Huddle.MaxPerHour = 1
	var h map[string]any
	apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "a", Brief: "b"}, &h)
	apiJSON(t, ts, "POST", "/api/huddle/"+h["id"].(string)+"/decline", nil, nil)
	var e map[string]string
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "a", Brief: "b"}, &e); code != http.StatusTooManyRequests {
		t.Fatalf("over limit = %d %v", code, e)
	}
	// The user may always call, and needs a session.
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{}, nil); code != http.StatusBadRequest {
		t.Fatalf("user call without session = %d", code)
	}
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{SessionID: "sess-1"}, &h); code != http.StatusCreated || h["origin"] != originUser {
		t.Fatalf("user call = %d %v", code, h)
	}
	if len(*rang) != 1 {
		t.Fatalf("a user call must not ring the phone: %v", *rang)
	}
	if !strings.Contains(srv.huddles.get(h["id"].(string)).Brief, "hi") {
		t.Fatalf("brief lacks the session transcript: %q", srv.huddles.get(h["id"].(string)).Brief)
	}

	srv.cfg.Huddle.MaxPerHour = 10
	srv.huddles.finish(srv.huddles.get(h["id"].(string)), hEnded, "x")
	srv.cfg.Huddle.QuietHours = "00:00-23:59"
	if code := apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{Topic: "a", Brief: "b"}, &e); code != http.StatusTooManyRequests || !strings.Contains(e["error"], "quiet") {
		t.Fatalf("quiet = %d %v", code, e)
	}
}

func TestInQuiet(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	for _, tc := range []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"22:00-08:00", at(23, 0), true},
		{"22:00-08:00", at(7, 59), true},
		{"22:00-08:00", at(8, 0), false},
		{"22:00-08:00", at(12, 0), false},
		{"12:00-13:00", at(12, 30), true},
		{"", at(3, 0), false},
		{"bad", at(3, 0), false},
	} {
		if got := inQuiet(tc.spec, tc.t); got != tc.want {
			t.Errorf("inQuiet(%q, %s) = %v", tc.spec, tc.t.Format("15:04"), got)
		}
	}
}

func TestVoipRegister(t *testing.T) {
	_, ts, _, _ := huddleServer(t)
	tok := strings.Repeat("ab", 32)
	if code := apiJSON(t, ts, "POST", "/api/push/voip/register", map[string]string{"token": tok, "bundle": "com.evil.app"}, nil); code != 400 {
		t.Fatalf("foreign bundle = %d", code)
	}
	if code := apiJSON(t, ts, "POST", "/api/push/voip/register", map[string]string{"token": "zz", "bundle": "com.teceer.harness"}, nil); code != 400 {
		t.Fatalf("bad token = %d", code)
	}
	if code := apiJSON(t, ts, "POST", "/api/push/voip/register", map[string]string{"token": strings.ToUpper(tok), "bundle": "com.teceer.harness.dev", "device": "iPhone"}, nil); code != 200 {
		t.Fatalf("register = %d", code)
	}
}

func TestAPNsRing(t *testing.T) {
	srv, _, _, _ := huddleServer(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPath := filepath.Join(srv.cfg.Home, "k.p8")
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	srv.cfg.Huddle = config.Huddle{APNsKey: keyPath, APNsKeyID: "KEY1", APNsTeamID: "TEAM1", RingSeconds: 45, MaxPerHour: 3}

	var gotProd, gotSandbox []*http.Request
	var bodies []string
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProd = append(gotProd, r)
		w.WriteHeader(400)
		w.Write([]byte(`{"reason":"BadDeviceToken"}`))
	}))
	defer prod.Close()
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSandbox = append(gotSandbox, r)
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
	}))
	defer sandbox.Close()
	oldP, oldS := apnsProduction, apnsSandbox
	apnsProduction, apnsSandbox = prod.URL, sandbox.URL
	defer func() { apnsProduction, apnsSandbox = oldP, oldS }()

	m := newHuddleManager(srv.cfg, srv.st)
	tok := strings.Repeat("cd", 32)
	m.voip.update(func(all map[string]voipDevice) { all[tok] = voipDevice{Bundle: "com.teceer.harness.dev"} })
	h := &huddle{ID: "id-1", Caller: "core11", Topic: "Temat"}
	if err := m.ring(h); err != nil {
		t.Fatal(err)
	}
	if len(gotProd) != 1 || len(gotSandbox) != 1 {
		t.Fatalf("prod=%d sandbox=%d", len(gotProd), len(gotSandbox))
	}
	r := gotSandbox[0]
	if r.URL.Path != "/3/device/"+tok || r.Header.Get("apns-topic") != "com.teceer.harness.dev.voip" || r.Header.Get("apns-push-type") != "voip" {
		t.Fatalf("request %s %v", r.URL.Path, r.Header)
	}
	if !strings.Contains(bodies[0], `"huddleId":"id-1"`) {
		t.Fatalf("payload %s", bodies[0])
	}
	// The JWT verifies with the key.
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("authorization"), "bearer "), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("jwt signature does not verify")
	}
	// The sandbox is remembered: the next ring goes there first.
	if !m.voip.all()[tok].Sandbox {
		t.Fatal("sandbox not remembered")
	}
	m.ring(h)
	if len(gotProd) != 1 || len(gotSandbox) != 2 {
		t.Fatalf("second ring: prod=%d sandbox=%d", len(gotProd), len(gotSandbox))
	}
}

func TestMCPToolsList(t *testing.T) {
	s := &mcpServer{}
	res, err := s.handle(rpcMsg{Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-06-18"}`)})
	if err != nil || res.(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %v %v", res, err)
	}
	res, _ = s.handle(rpcMsg{Method: "tools/list"})
	b, _ := json.Marshal(res)
	for _, name := range []string{"huddle_call", "huddle_answer", "huddle_end"} {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Errorf("tools/list lacks %s", name)
		}
	}
}

func TestJoinDelta(t *testing.T) {
	for _, c := range []struct{ prev, delta, want string }{
		{"", " Cześć", "Cześć"},
		{"na demo.", "Świetnie", "na demo. Świetnie"},
		{"Do usłyszenia!", " Do", "Do usłyszenia! Do"},
		{"przygo", "tuję", "przygotuję"},
		{"Tak,", " robimy", "Tak, robimy"},
	} {
		if got := joinDelta(c.prev, c.delta); got != c.want {
			t.Errorf("joinDelta(%q, %q) = %q, want %q", c.prev, c.delta, got, c.want)
		}
	}
}

func TestSystemPromptKeepsPolishAndDelegates(t *testing.T) {
	m := &huddleManager{}
	for _, origin := range []string{originAgent, originUser} {
		p := m.systemPrompt(&huddle{Origin: origin, Caller: "s", Topic: "t"})
		for _, want := range []string{"wyłącznie po polsku", "nie pytaj o zmianę języka", "nigdy nie mów, że agent czegoś nie może", "po wyraźnym potwierdzeniu", "nie zapisuj jej słów jako decyzji"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt lacks %q", origin, want)
			}
		}
	}
}
