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
	want := []Message{{"user", "fix login"}, {"assistant", "Fixed."}, {"user", "thanks"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("msg %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, _ := Tail(p, 2); len(got) != 2 || got[1].Text != "thanks" {
		t.Errorf("max 2 should keep the newest: %+v", got)
	}
}
