package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/coder/websocket"
	"github.com/teceer/harness/internal/config"
)

// The voice of a huddle is a Gemini Live session: audio in (16 kHz PCM),
// audio out (24 kHz PCM), transcripts of both sides and function calls
// for asking the coding agent. Docs: ai.google.dev/gemini-api/docs/live.

var geminiLiveURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

func geminiKey(h config.Huddle) (string, error) {
	if b, err := os.ReadFile(h.GeminiKeyFile); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	}
	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("no Gemini API key: put it in %s", h.GeminiKeyFile)
}

type liveTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func stringParams(required bool, props map[string]string) map[string]any {
	p := map[string]any{}
	var req []string
	for k, d := range props {
		p[k] = map[string]string{"type": "STRING", "description": d}
		req = append(req, k)
	}
	out := map[string]any{"type": "OBJECT", "properties": p}
	if required {
		out["required"] = req
	}
	return out
}

// liveServerMsg is the part of a server message a huddle uses.
type liveServerMsg struct {
	SetupComplete *struct{} `json:"setupComplete"`
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     []byte `json:"data"` // base64 in JSON
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
		InputTranscription  *struct{ Text string } `json:"inputTranscription"`
		OutputTranscription *struct{ Text string } `json:"outputTranscription"`
		Interrupted         bool                   `json:"interrupted"`
		TurnComplete        bool                   `json:"turnComplete"`
	} `json:"serverContent"`
	ToolCall *struct {
		FunctionCalls []struct {
			ID   string            `json:"id"`
			Name string            `json:"name"`
			Args map[string]string `json:"args"`
		} `json:"functionCalls"`
	} `json:"toolCall"`
	SessionResumptionUpdate *struct {
		NewHandle string `json:"newHandle"`
		Resumable bool   `json:"resumable"`
	} `json:"sessionResumptionUpdate"`
	GoAway *struct{} `json:"goAway"`
}

// dialLive opens a session; handle resumes an earlier one (a connection
// lasts about ten minutes, the conversation goes on across them).
func dialLive(ctx context.Context, h config.Huddle, system string, tools []liveTool, handle string) (*websocket.Conn, error) {
	key, err := geminiKey(h)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{"x-goog-api-key": {key}}
	c, _, err := websocket.Dial(ctx, geminiLiveURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, fmt.Errorf("gemini live: %w", err)
	}
	c.SetReadLimit(8 << 20)
	resume := map[string]any{}
	if handle != "" {
		resume["handle"] = handle
	}
	setup := map[string]any{"setup": map[string]any{
		"model": "models/" + h.Model,
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig":  map[string]any{"prebuiltVoiceConfig": map[string]string{"voiceName": h.Voice}},
				"languageCode": "pl-PL",
			},
		},
		"systemInstruction":        map[string]any{"parts": []map[string]string{{"text": system}}},
		"tools":                    []map[string]any{{"functionDeclarations": tools}},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
		"contextWindowCompression": map[string]any{"slidingWindow": map[string]any{}},
		"sessionResumption":        resume,
	}}
	if err := writeJSONWS(ctx, c, setup); err != nil {
		c.CloseNow()
		return nil, err
	}
	// The first answer is setupComplete (or the close reason of a bad setup).
	_, b, err := c.Read(ctx)
	if err != nil {
		c.CloseNow()
		return nil, fmt.Errorf("gemini live setup: %w", err)
	}
	var m liveServerMsg
	if json.Unmarshal(b, &m) != nil || m.SetupComplete == nil {
		c.CloseNow()
		return nil, errors.New("gemini live: unexpected setup answer: " + trim(string(b), 200))
	}
	return c, nil
}

// wsWriter is either side of a huddle: the phone or the voice.
type wsWriter interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
}

func writeJSONWS(ctx context.Context, c wsWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
