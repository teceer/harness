package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"fix login"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","name":"Bash"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>x</local-command-caveat>"}}
{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed."}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"thanks"}]}}
`), 0o644)
	got, err := Tail(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{{Role: "user", Text: "fix login"}, {Role: "assistant", Text: "Fixed."}, {Role: "user", Text: "thanks"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Text != want[i].Text {
			t.Errorf("msg %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, _ := Tail(p, 2); len(got) != 2 || got[1].Text != "thanks" {
		t.Errorf("max 2 should keep the newest: %+v", got)
	}
}

func TestTailImages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	big := make([]byte, maxImage+1)
	for i := range big {
		big[i] = 'A'
	}
	// the oversized one is skipped, the small one comes through
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+string(big)+`"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"see this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}}
`), 0o644)
	got, err := Tail(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "see this" || len(got[0].Images) != 1 || got[0].Images[0].MediaType != "image/png" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Images[0].Data != "iVBORw0KGgo=" {
		t.Errorf("image data = %q", got[0].Images[0].Data)
	}
}
