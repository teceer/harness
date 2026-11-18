package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	gosync "sync"
	"syscall"
	"time"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/teceer/harness/internal/ansi"
	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/tmux"
)

// cmdRemoteAttach shows a remote session in this terminal: it mirrors the
// session's tmux pane as its host streams it (GET …/pane) and passes on
// what is typed (POST …/keys), so the session is used as if it ran here.
// The sidebar runs it in a pane of the harness tmux for a remote session;
// it works in any terminal too. ctrl+] leaves; the session keeps running.
//
// Only the keys the remote's API allows travel (text, the ordinary named
// keys, Ctrl/Alt with a letter): the same limits as the web remote.
func cmdRemoteAttach(args []string) error {
	if len(args) != 1 || !isRemoteID(args[0]) {
		return errors.New("usage: harness remote-attach <remote>:<id>")
	}
	cfg, err := config.Load(false)
	if err != nil {
		return err
	}
	host, rid, _ := splitRemoteID(args[0])
	c, err := remoteClientFor(cfg, host)
	if err != nil {
		return err
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("remote-attach needs a terminal: %w", err)
	}
	// Alternate screen, no cursor (Claude draws its own), bracketed paste
	// (a paste is one text event, not a run of Enters) and wheel reports.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[?1000h\x1b[?1006h")
	defer func() {
		os.Stdout.WriteString("\x1b[?1006l\x1b[?1000l\x1b[?2004l\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &mirror{title: host + ":" + short(rid), note: "connecting…"}
	m.draw()

	// The pane, as its host shows it. A lost stream is retried for as long
	// as the mirror is open: a link that drops must not close the window.
	go func() {
		for ctx.Err() == nil {
			err := c.sse(ctx, sessionPath(rid, "pane")+"?format=spans", func(data []byte) {
				var frame struct {
					Lines [][]ansi.Seg `json:"lines"`
				}
				if json.Unmarshal(data, &frame) == nil {
					m.set(frame.Lines, "")
				}
			})
			if ctx.Err() != nil {
				return
			}
			msg := "stream ended"
			if err != nil {
				msg = err.Error()
			}
			m.set(nil, msg+" — retrying")
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			m.draw()
		}
	}()

	// Typing is sent in order, one request at a time; what arrives while
	// one is under way goes out together in the next.
	keys := make(chan tmux.KeyEvent, 1024)
	go func() {
		for ev := range keys {
			batch := []tmux.KeyEvent{ev}
		more:
			for len(batch) < maxLiveEvents {
				select {
				case ev := <-keys:
					batch = append(batch, ev)
				default:
					break more
				}
			}
			if err := c.call(http.MethodPost, sessionPath(rid, "keys"), map[string]any{"events": mergeText(batch)}, nil); err != nil {
				m.set(nil, "not sent: "+err.Error())
			}
		}
	}()

	var p keyParser
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		events, wheel, quit := p.feed(buf[:n])
		for _, ev := range events {
			select {
			case keys <- ev:
			default: // the link is stuck and the queue full: drop rather than block the terminal
			}
		}
		if wheel != 0 {
			// Like the wheel over a local session: only a full-screen one scrolls.
			go c.call(http.MethodPost, sessionPath(rid, "scroll"), map[string]int{"lines": wheel * 3}, nil)
		}
		if quit {
			return nil
		}
	}
}

// mergeText joins neighbouring text events, so a burst of typing is one
// send-keys on the other side.
func mergeText(events []tmux.KeyEvent) []tmux.KeyEvent {
	out := events[:0:0]
	for _, ev := range events {
		if n := len(out); n > 0 && ev.Text != "" && out[n-1].Text != "" {
			out[n-1].Text += ev.Text
			continue
		}
		out = append(out, ev)
	}
	return out
}

// ---- screen ----

// mirror is the last frame of the remote pane and how it is drawn here.
type mirror struct {
	mu    gosync.Mutex
	title string
	note  string // connection trouble, shown in the top line
	lines [][]ansi.Seg
}

// set replaces the frame (nil keeps it) and the note, and redraws.
func (m *mirror) set(lines [][]ansi.Seg, note string) {
	m.mu.Lock()
	if lines != nil {
		m.lines = lines
	}
	m.note = note
	m.mu.Unlock()
	m.draw()
}

func (m *mirror) draw() {
	w, h := termSize()
	m.mu.Lock()
	defer m.mu.Unlock()
	os.Stdout.WriteString(renderMirror(m.lines, m.title, m.note, w, h))
}

// renderMirror lays the frame out for a w×h terminal: one line saying
// what this is, then the pane. The remote pane keeps its own size, so a
// larger one is cut on the right and shows its bottom rows (where the
// prompt is). Lines are rewritten in place, without clearing the screen
// first, so nothing flickers.
func renderMirror(lines [][]ansi.Seg, title, note string, w, h int) string {
	w, h = max(w, 10), max(h, 2)
	head := " ⇄ " + title + " · remote · ctrl+] detach"
	if note != "" {
		head = " ⇄ " + title + " · " + note
	}
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[0;2;7m")
	b.WriteString(xansi.Truncate(head, w, "…"))
	b.WriteString("\x1b[K\x1b[0m")

	room := h - 1
	if len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	for i := range room {
		b.WriteString("\r\n")
		if i < len(lines) {
			var row strings.Builder
			for _, g := range lines[i] {
				row.WriteString(sgr(g))
				row.WriteString(g.Text)
			}
			b.WriteString(xansi.Truncate(row.String(), w, ""))
		}
		b.WriteString("\x1b[0m\x1b[K")
	}
	return b.String()
}

// sgr is the escape sequence that sets a segment's style from scratch.
func sgr(g ansi.Seg) string {
	p := []string{"0"}
	if g.Bold {
		p = append(p, "1")
	}
	if g.Dim {
		p = append(p, "2")
	}
	if g.Italic {
		p = append(p, "3")
	}
	if g.Underline {
		p = append(p, "4")
	}
	if c, ok := rgb(g.FG); ok {
		p = append(p, "38;2;"+c)
	}
	if c, ok := rgb(g.BG); ok {
		p = append(p, "48;2;"+c)
	}
	return "\x1b[" + strings.Join(p, ";") + "m"
}

// rgb turns "#rrggbb" (how internal/ansi names every colour) into "r;g;b".
func rgb(hex string) (string, bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return "", false
	}
	n, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d;%d;%d", n>>16&0xff, n>>8&0xff, n&0xff), true
}

// ---- keyboard ----

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// keyParser turns raw terminal input into the key events the remote's
// /keys accepts (liveKeyRe). It keeps what a read cut in half — a UTF-8
// character, an escape sequence, a paste — for the next one.
type keyParser struct {
	pend    []byte
	inPaste bool
	paste   []byte
}

// feed parses one read. wheel is the mouse wheel in notches (negative:
// up); quit reports ctrl+].
func (p *keyParser) feed(in []byte) (events []tmux.KeyEvent, wheel int, quit bool) {
	b := append(p.pend, in...)
	p.pend = nil
	var text []byte
	flush := func() {
		if len(text) > 0 {
			events = append(events, tmux.KeyEvent{Text: string(text)})
			text = nil
		}
	}
	key := func(k string) {
		flush()
		events = append(events, tmux.KeyEvent{Key: k})
	}
	defer flush()

	for i := 0; i < len(b); {
		if p.inPaste {
			end := bytes.Index(b[i:], pasteEnd)
			if end < 0 {
				// Hold back a tail that may be the start of the end marker.
				keep := 0
				for k := min(len(pasteEnd)-1, len(b)-i); k > 0; k-- {
					if bytes.HasPrefix(pasteEnd, b[len(b)-k:]) {
						keep = k
						break
					}
				}
				p.paste = append(p.paste, b[i:len(b)-keep]...)
				p.pend = append(p.pend, b[len(b)-keep:]...)
				return
			}
			p.paste = append(p.paste, b[i:i+end]...)
			i += end + len(pasteEnd)
			// Terminals paste line ends as CR; the session wants them as text.
			text = append(text, bytes.ReplaceAll(bytes.ReplaceAll(p.paste, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))...)
			p.inPaste, p.paste = false, nil
			continue
		}
		c := b[i]
		switch {
		case c == 0x1d: // ctrl+]
			return events, wheel, true
		case c == 0x1b:
			n, k, wh := parseEscape(b[i:])
			if n == 0 { // cut in half: wait for the rest
				p.pend = append(p.pend, b[i:]...)
				return
			}
			i += n
			wheel += wh
			switch k {
			case "":
			case "paste":
				p.inPaste = true
			default:
				key(k)
			}
		case c == '\r':
			key("Enter")
			i++
		case c == '\t':
			key("Tab")
			i++
		case c == 0x7f:
			key("BSpace")
			i++
		case c >= 0x01 && c <= 0x1a: // ctrl+letter; ctrl+j (LF) is a newline in Claude
			key("C-" + string(rune('a'+c-1)))
			i++
		case c < 0x20: // NUL and the rest of C0 have no key the remote takes
			i++
		default:
			if !utf8.FullRune(b[i:]) && len(b)-i < utf8.UTFMax {
				p.pend = append(p.pend, b[i:]...)
				return
			}
			_, size := utf8.DecodeRune(b[i:])
			text = append(text, b[i:i+size]...)
			i += size
		}
	}
	return
}

// parseEscape reads one escape sequence at the start of b (b[0] is ESC).
// n is how many bytes it took (0: incomplete), key the tmux key name, ""
// for a sequence that is dropped and "paste" for the start of a paste.
func parseEscape(b []byte) (n int, key string, wheel int) {
	if len(b) == 1 {
		return 1, "Escape", 0
	}
	switch c := b[1]; {
	case c == '[':
		end := 2
		for end < len(b) && (b[end] < 0x40 || b[end] > 0x7e) {
			end++
		}
		if end == len(b) {
			if len(b) > 64 { // not a sequence at all
				return 2, "", 0
			}
			return 0, "", 0
		}
		params, final := string(b[2:end]), b[end]
		alt := strings.HasSuffix(params, ";3")
		first, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(params, "<"), ";", 2)[0])
		switch final {
		case 'A':
			key = "Up"
		case 'B':
			key = "Down"
		case 'C':
			key = "Right"
		case 'D':
			key = "Left"
		case 'H':
			key = "Home"
		case 'F':
			key = "End"
		case 'Z':
			key = "BTab"
		case '~':
			key = map[int]string{1: "Home", 7: "Home", 4: "End", 8: "End", 3: "DC", 5: "PageUp", 6: "PageDown", 200: "paste"}[first]
		case 'M', 'm':
			if strings.HasPrefix(params, "<") && final == 'M' { // SGR mouse: only the wheel matters
				wheel = map[int]int{64: -1, 65: 1}[first]
			}
		}
		if alt && (key == "Left" || key == "Right") {
			key = "M-" + key
		}
		return end + 1, key, wheel
	case c == 'O':
		if len(b) < 3 {
			return 0, "", 0
		}
		return 3, map[byte]string{'A': "Up", 'B': "Down", 'C': "Right", 'D': "Left", 'H': "Home", 'F': "End"}[b[2]], 0
	case c == '\r':
		return 2, "M-Enter", 0
	case c == 0x7f:
		return 2, "M-BSpace", 0
	case c >= 'a' && c <= 'z':
		return 2, "M-" + string(rune(c)), 0
	}
	return 1, "Escape", 0
}
