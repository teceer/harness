package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestHuddleGeminiLive talks to the real Gemini Live: a phone sends a
// spoken Polish question and the voice must answer and ask the agent.
// Run with HARNESS_LIVE_WAV=<16 kHz mono 16-bit wav> and GEMINI_API_KEY.
func TestHuddleGeminiLive(t *testing.T) {
	wav := os.Getenv("HARNESS_LIVE_WAV")
	if wav == "" || os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("HARNESS_LIVE_WAV / GEMINI_API_KEY not set")
	}
	audio, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	audio = audio[44:] // RIFF header
	srv, ts, _, _ := huddleServer(t)
	cfg := srv.cfg.Huddle
	cfg.GeminiKeyFile = "/nonexistent"
	cfg.Model, cfg.Voice = "gemini-3.8-live", "Puck"
	srv.huddles.dial = func(ctx context.Context, system string, tools []liveTool, handle string) (liveConn, error) {
		c, err := dialLive(ctx, cfg, system, tools, handle)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	var h map[string]any
	apiJSON(t, ts, "POST", "/api/huddle", huddleRequest{SessionID: "sess-1", Topic: "Test huddle",
		Brief: "Pracujesz nad funkcją huddle w harness. Nie znasz nazwy brancha: o to trzeba zapytać agenta.", Questions: []string{"Czy test działa?"}}, &h)
	id := h["id"].(string)
	phone := dialPhone(t, ts, id)
	defer phone.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() {
		time.Sleep(6 * time.Second)             // let the greeting play
		for i := 0; i < len(audio); i += 1280 { // 40 ms frames in real time
			end := min(i+1280, len(audio))
			phone.Write(ctx, websocket.MessageBinary, audio[i:end])
			time.Sleep(40 * time.Millisecond)
		}
		silence := make([]byte, 1280)
		for i := 0; i < 75; i++ {
			phone.Write(ctx, websocket.MessageBinary, silence)
			time.Sleep(40 * time.Millisecond)
		}
	}()
	go func() {
		var ev agentEvent
		apiJSON(t, ts, "GET", "/api/huddle/"+id+"/next?wait=55", nil, &ev)
		t.Logf("agent got: %+v", ev)
		if ev.Type == "question" {
			apiJSON(t, ts, "POST", "/api/huddle/"+id+"/answer", map[string]string{"text": "Branch feature/huddle."}, nil)
		}
	}()
	var audioBytes int
	var model, user, agent strings.Builder
	for ctx.Err() == nil {
		mt, b, err := phone.Read(ctx)
		if err != nil {
			break
		}
		if mt == websocket.MessageBinary {
			audioBytes += len(b)
			continue
		}
		s := string(b)
		switch {
		case strings.Contains(s, `"role":"model"`):
			model.WriteString(jsonField(s, "text"))
		case strings.Contains(s, `"role":"user"`):
			user.WriteString(jsonField(s, "text"))
		case strings.Contains(s, `"role":"agent"`):
			agent.WriteString(jsonField(s, "text") + " | ")
		}
		if strings.Contains(agent.String(), "feature/huddle") && strings.Contains(strings.ToLower(model.String()), "huddle") {
			time.Sleep(3 * time.Second)
			break
		}
	}
	t.Logf("audio %.1fs\nmodel: %s\nuser: %s\nagent: %s", float64(audioBytes)/48000, model.String(), user.String(), agent.String())
	if audioBytes < 48000 || model.Len() == 0 || user.Len() == 0 {
		t.Fatal("no conversation")
	}
	if !strings.Contains(agent.String(), "pytanie do agenta") {
		t.Fatal("the voice did not ask the agent")
	}
}

func jsonField(s, k string) string {
	i := strings.Index(s, `"`+k+`":"`)
	if i < 0 {
		return ""
	}
	s = s[i+len(k)+4:]
	return s[:strings.Index(s, `"`)]
}
