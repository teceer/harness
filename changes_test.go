package main

import (
	"reflect"
	"testing"
)

func TestParseStatus(t *testing.T) {
	out := "## feature/x...origin/feature/x [ahead 2, behind 1]\x00" +
		" M ui.go\x00" +
		"A  changes.go\x00" +
		"R  new/name.go\x00old/name.go\x00" +
		"?? notes.md\x00" +
		"UU conflict.go\x00"
	branch, files := parseStatus([]byte(out))
	if branch != "feature/x ↑2 ↓1" {
		t.Errorf("branch = %q", branch)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.code+" "+f.path)
	}
	want := []string{"A  changes.go", "UU conflict.go", "R  new/name.go", "?? notes.md", " M ui.go"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("files = %q, want %q", paths, want)
	}
}

func TestParseBranch(t *testing.T) {
	for in, want := range map[string]string{
		"main":                          "main",
		"main...origin/main":            "main",
		"main...origin/main [behind 3]": "main ↓3",
		"No commits yet on main":        "main",
		"HEAD (no branch)":              "HEAD (no branch)",
	} {
		if got := parseBranch(in); got != want {
			t.Errorf("parseBranch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseNumstat(t *testing.T) {
	out := "10\t2\tui.go\x00" +
		"-\t-\tlogo.png\x00" +
		"3\t1\t\x00old/name.go\x00new/name.go\x00" +
		"0\t7\tgone.go\x00"
	got := parseNumstat([]byte(out))
	want := map[string]numstat{
		"ui.go":       {10, 2, false},
		"logo.png":    {0, 0, true},
		"new/name.go": {3, 1, false},
		"gone.go":     {0, 7, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNumstat = %v, want %v", got, want)
	}
}

func TestCodeLetter(t *testing.T) {
	for code, want := range map[string]byte{
		" M": 'M', "M ": 'M', "MM": 'M', "A ": 'A', "AM": 'M',
		"??": '?', "D ": 'D', " D": 'D', "R ": 'R', "UU": 'U', "AA": 'U',
	} {
		if got := codeLetter(code); got != want {
			t.Errorf("codeLetter(%q) = %c, want %c", code, got, want)
		}
	}
}

func TestTruncateLeft(t *testing.T) {
	if got := truncateLeft("internal/tmux", 6); got != "…/tmux" {
		t.Errorf("got %q", got)
	}
	if got := truncateLeft("web", 6); got != "web" {
		t.Errorf("got %q", got)
	}
}
