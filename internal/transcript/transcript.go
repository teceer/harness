// Package transcript reads the tail of a Claude Code session JSONL file to
// recover the AI-generated title and the last assistant text.
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// tailBytes bounds how much of a (possibly huge) transcript we read.
const tailBytes = 512 << 10

type Info struct {
	Title       string
	LastMessage string
}

type entry struct {
	Type    string `json:"type"`
	AITitle string `json:"aiTitle"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsSidechain bool `json:"isSidechain"`
}

// Read scans the last tailBytes of the transcript, newest line first.
func Read(path string) (Info, error) {
	var info Info
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return info, err
	}
	off := st.Size() - tailBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return info, err
	}
	if off > 0 { // drop the partial first line
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}

	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0 && (info.Title == "" || info.LastMessage == ""); i-- {
		line := lines[i]
		if len(line) == 0 {
			continue
		}
		// Cheap pre-filter before paying for a full decode.
		isTitle := bytes.Contains(line, []byte(`"ai-title"`))
		isAssistant := bytes.Contains(line, []byte(`"assistant"`))
		if !isTitle && !isAssistant {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch {
		case e.Type == "ai-title" && info.Title == "":
			info.Title = strings.TrimSpace(e.AITitle)
		case e.Type == "assistant" && !e.IsSidechain && info.LastMessage == "":
			info.LastMessage = assistantText(e.Message.Content)
		}
	}
	return info, nil
}

func assistantText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// Message is one turn of the visible conversation.
type Message struct {
	Role string // "user" or "assistant"
	Text string
}

// tailPreview bounds the read for Tail; previews need only the last turns.
const tailPreview = 2 << 20

// Tail returns up to max of the last conversation messages, oldest first:
// what the user typed and what Claude answered in text. Tool traffic,
// thinking, meta entries, slash-command echoes and subagent threads are
// left out.
func Tail(path string, max int) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := st.Size() - tailPreview
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	var out []Message
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0 && len(out) < max; i-- {
		var e struct {
			entry
			IsMeta bool `json:"isMeta"`
		}
		if len(lines[i]) == 0 || json.Unmarshal(lines[i], &e) != nil || e.IsSidechain || e.IsMeta {
			continue
		}
		var text string
		switch e.Type {
		case "assistant":
			text = assistantText(e.Message.Content)
		case "user":
			text = userText(e.Message.Content)
		}
		if text != "" {
			out = append(out, Message{Role: e.Type, Text: text})
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// userText is what the user typed; tool results and tagged echoes of
// slash commands are not.
func userText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "<") {
			return ""
		}
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<") {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}
