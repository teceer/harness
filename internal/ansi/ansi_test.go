package ansi

import (
	"strings"
	"testing"
)

func TestHTML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello", "hello"},
		{"escapes html", "a<b>&c", "a&lt;b&gt;&amp;c"},
		{"truecolor", "\x1b[38;2;122;162;255mx\x1b[0m", `<span style="color:#7aa2ff;">x</span>`},
		{"256 colour", "\x1b[38;5;196mx\x1b[39m", `<span style="color:#ff0000;">x</span>`},
		{"background", "\x1b[48;5;236m \x1b[49m", `<span style="background:#303030;"> </span>`},
		{"bold and reset", "\x1b[1mb\x1b[22mn", "<span style=\"font-weight:700;\">b</span>n"},
		{"basic colours", "\x1b[31mr\x1b[0m", `<span style="color:#ff5c8a;">r</span>`},
		{"cursor moves dropped", "a\x1b[2Jb\x1b[Hc", "abc"},
		{"osc dropped", "a\x1b]0;title\x07b", "ab"},
		{"unfinished escape", "a\x1b[", "a"},
	}
	for _, tc := range cases {
		if got := HTML(tc.in); got != tc.want {
			t.Errorf("%s: HTML(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestHTMLRealCapture(t *testing.T) {
	// what `tmux capture-pane -e` gives for a styled sidebar row
	in := "\x1b[48;2;38;48;79m \x1b[1m\x1b[38;2;255;92;138m◆\x1b[0m session\x1b[39m"
	got := HTML(in)
	if !strings.Contains(got, "background:#26304f") || !strings.Contains(got, "color:#ff5c8a") ||
		!strings.Contains(got, "font-weight:700") || !strings.Contains(got, "◆") ||
		!strings.Contains(got, "session") {
		t.Errorf("got %q", got)
	}
	if strings.Count(got, "<span") != strings.Count(got, "</span>") {
		t.Errorf("unbalanced spans: %q", got)
	}
}
