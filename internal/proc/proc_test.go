package proc

import "testing"

func TestIsClaudeArgs(t *testing.T) {
	for args, want := range map[string]bool{
		"/Users/x/.local/bin/claude":                               true,
		"claude --resume 6bd01cfa":                                 true,
		"/Users/x/.local/bin/claude daemon run --origin transient": false,
		"/bin/zsh -c claude":                                       false,
		"node /usr/lib/claude-helper":                              false,
	} {
		if got := IsClaudeArgs(args); got != want {
			t.Errorf("IsClaudeArgs(%q) = %v, want %v", args, got, want)
		}
	}
}
