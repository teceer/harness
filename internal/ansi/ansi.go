// Package ansi renders terminal output as HTML, so a phone can watch a
// tmux pane live (`tmux capture-pane -e` keeps the colours as SGR codes).
package ansi

import (
	"fmt"
	"html"
	"strconv"
	"strings"
)

type style struct {
	fg, bg                 string
	bold, dim, ital, under bool
}

func (s style) css() string {
	var b strings.Builder
	if s.fg != "" {
		fmt.Fprintf(&b, "color:%s;", s.fg)
	}
	if s.bg != "" {
		fmt.Fprintf(&b, "background:%s;", s.bg)
	}
	if s.bold {
		b.WriteString("font-weight:700;")
	}
	if s.dim {
		b.WriteString("opacity:.65;")
	}
	if s.ital {
		b.WriteString("font-style:italic;")
	}
	if s.under {
		b.WriteString("text-decoration:underline;")
	}
	return b.String()
}

// HTML converts terminal output to HTML spans. Everything that is not an
// SGR (colour/attribute) sequence is dropped: cursor moves and the like
// mean nothing in a static snapshot.
func HTML(s string) string {
	var out strings.Builder
	var cur style
	open := false
	flush := func() {
		if open {
			out.WriteString("</span>")
			open = false
		}
	}
	write := func(text string) {
		if text == "" {
			return
		}
		if css := cur.css(); css != "" {
			if !open {
				fmt.Fprintf(&out, `<span style="%s">`, css)
				open = true
			}
		} else {
			flush()
		}
		out.WriteString(html.EscapeString(text))
	}

	for i := 0; i < len(s); {
		c := s[i]
		if c != 0x1b {
			j := strings.IndexByte(s[i:], 0x1b)
			if j < 0 {
				write(s[i:])
				break
			}
			write(s[i : i+j])
			i += j
			continue
		}
		seq, next := escape(s, i)
		i = next
		if strings.HasSuffix(seq, "m") && strings.HasPrefix(seq, "\x1b[") {
			flush() // the style changes: close the current span
			cur = apply(cur, seq[2:len(seq)-1])
		}
	}
	flush()
	return out.String()
}

// escape returns the escape sequence starting at i and the index after it.
func escape(s string, i int) (string, int) {
	if i+1 >= len(s) {
		return s[i:], len(s)
	}
	switch s[i+1] {
	case '[': // CSI: parameters then a final byte
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return s[i : j+1], j + 1
			}
		}
		return s[i:], len(s)
	case ']': // OSC: ends with BEL or ST
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return s[i : j+1], j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return s[i : j+2], j + 2
			}
		}
		return s[i:], len(s)
	}
	return s[i : i+2], i + 2
}

func apply(cur style, params string) style {
	if params == "" {
		return style{}
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			cur = style{}
		case n == 1:
			cur.bold = true
		case n == 2:
			cur.dim = true
		case n == 3:
			cur.ital = true
		case n == 4:
			cur.under = true
		case n == 22:
			cur.bold, cur.dim = false, false
		case n == 23:
			cur.ital = false
		case n == 24:
			cur.under = false
		case n == 39:
			cur.fg = ""
		case n == 49:
			cur.bg = ""
		case n == 38 || n == 48:
			color, used := readColor(fields[i+1:])
			if n == 38 {
				cur.fg = color
			} else {
				cur.bg = color
			}
			i += used
		case n >= 30 && n <= 37:
			cur.fg = palette[n-30]
		case n >= 40 && n <= 47:
			cur.bg = palette[n-40]
		case n >= 90 && n <= 97:
			cur.fg = palette[n-90+8]
		case n >= 100 && n <= 107:
			cur.bg = palette[n-100+8]
		}
	}
	return cur
}

// readColor reads the arguments of 38/48: "5;n" (256 colours) or
// "2;r;g;b" (truecolor), and reports how many it used.
func readColor(rest []string) (string, int) {
	if len(rest) == 0 {
		return "", 0
	}
	switch rest[0] {
	case "5":
		if len(rest) < 2 {
			return "", 1
		}
		n, _ := strconv.Atoi(rest[1])
		return color256(n), 2
	case "2":
		if len(rest) < 4 {
			return "", len(rest)
		}
		r, _ := strconv.Atoi(rest[1])
		g, _ := strconv.Atoi(rest[2])
		b, _ := strconv.Atoi(rest[3])
		return fmt.Sprintf("#%02x%02x%02x", r, g, b), 4
	}
	return "", 1
}

func color256(n int) string {
	switch {
	case n < 16:
		return palette[n]
	case n < 232:
		n -= 16
		v := []int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x", v[n/36], v[(n/6)%6], v[n%6])
	case n < 256:
		g := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
	return ""
}

// palette is the usual 16-colour set, in the tones the sidebar uses.
var palette = [16]string{
	"#1a1b26", "#ff5c8a", "#5ee38f", "#ffb547", "#7aa2ff", "#c792ea", "#4fd6be", "#a3adc8",
	"#5a6280", "#ff7eb6", "#9ece6a", "#ffd580", "#9cc4ff", "#e0b0ff", "#7be0d0", "#e6e9f2",
}
