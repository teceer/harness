package main

import (
	"reflect"
	"testing"

	"github.com/charmbracelet/x/ansi"
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

func TestParseNameStatus(t *testing.T) {
	out := "M\x00ui.go\x00R087\x00old.go\x00new.go\x00A\x00added.go\x00D\x00gone.go\x00"
	var got []string
	for _, f := range parseNameStatus([]byte(out)) {
		got = append(got, f.code+f.path)
	}
	want := []string{"M ui.go", "R new.go", "A added.go", "D gone.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseDiff(t *testing.T) {
	out := `diff --git a/x.go b/x.go
index 1..2 100644
--- a/x.go
+++ b/x.go
@@ -10,4 +10,5 @@ func main() {
 	a := 1
-	b := compute(a)
+	b := computeFast(a)
+	c := 3
 	return
\ No newline at end of file
`
	d := parseDiff(out)
	var kinds []byte
	for _, l := range d.lines {
		kinds = append(kinds, l.kind)
	}
	if string(kinds) != "@ -++ \\" {
		t.Fatalf("kinds = %q", kinds)
	}
	if h := d.lines[0]; h.old != 10 || h.new != 10 || h.text != "func main() {" {
		t.Errorf("hunk = %+v", h)
	}
	if l := d.lines[4]; l.new != 12 || l.old != 0 {
		t.Errorf("added line numbers = %d/%d", l.old, l.new)
	}
	if l := d.lines[5]; l.old != 12 || l.new != 13 || l.text != "    return" {
		t.Errorf("context line = %+v", l)
	}
	// compute → computeFast is marked as a whole word on both sides
	del, add := d.lines[2], d.lines[3]
	if got := string([]rune(del.text)[del.word[0]:del.word[1]]); got != "compute" {
		t.Errorf("removed word = %q", got)
	}
	if got := string([]rune(add.text)[add.word[0]:add.word[1]]); got != "computeFast" {
		t.Errorf("added word = %q", got)
	}
}

func TestWordRangesSkipsRewrites(t *testing.T) {
	a, b := wordRanges("completely different", "nothing alike here")
	if a != ([2]int{}) || b != ([2]int{}) {
		t.Errorf("rewrite marked: %v %v", a, b)
	}
}

func TestBarBlocks(t *testing.T) {
	for _, c := range []struct{ add, del, g, r int }{
		{10, 0, 5, 0}, {0, 10, 0, 5}, {5, 5, 3, 2}, {1, 0, 1, 0}, {1, 1, 1, 1}, {100, 1, 4, 1}, {0, 0, 0, 0},
	} {
		if g, r := barBlocks(c.add, c.del); g != c.g || r != c.r {
			t.Errorf("barBlocks(%d, %d) = %d, %d; want %d, %d", c.add, c.del, g, r, c.g, c.r)
		}
		if n := len([]rune(ansi.Strip(statBar(c.add, c.del)))); n != 5 {
			t.Errorf("statBar(%d, %d) is %d wide", c.add, c.del, n)
		}
	}
}
