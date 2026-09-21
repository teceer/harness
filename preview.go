package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/transcript"
)

// cmdPreview prints the recent conversation of a session. With --hold it is
// the body of the hold-space peek: it stays up while space is held, ↑/↓
// (k/j) walk the --ids list (the sidebar's order), and it exits as soon as
// space is let go or any other key is pressed. The session on screen is
// written to --result and the sidebar is nudged, so its selection follows.
func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	hold := fs.Bool("hold", false, "stay open while space is held")
	idList := fs.String("ids", "", "comma-separated sessions ↑/↓ move through")
	result := fs.String("result", "", "file receiving the id on screen")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: harness preview [--hold [--ids a,b,…] [--result file]] <id>")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		if !*hold {
			s, err := st.Find(fs.Arg(0))
			if err != nil {
				return err
			}
			w, h := termSize()
			fmt.Println(renderPreview(s, w, h, false, ""))
			return nil
		}
		lipgloss.SetColorProfile(termenv.TrueColor) // inside the harness tmux

		ids := []string{fs.Arg(0)}
		if *idList != "" {
			ids = strings.Split(*idList, ",")
		}
		at := 0
		for i, id := range ids {
			if id == fs.Arg(0) {
				at = i
			}
		}
		draw := func() {
			s, err := st.Find(ids[at])
			w, h := termSize()
			page := stErr.Render(fmt.Sprint(err))
			if err == nil {
				pos := ""
				if len(ids) > 1 {
					pos = fmt.Sprintf("%d/%d", at+1, len(ids))
				}
				page = renderPreview(s, w, h, true, pos)
			}
			fmt.Print("\x1b[H\x1b[2J" + page)
		}
		fmt.Print("\x1b[?25l") // hide the cursor
		defer fmt.Print("\x1b[?25h")
		draw()
		return holdWhileSpace(func(delta int) {
			next := min(max(at+delta, 0), len(ids)-1)
			if next == at {
				return
			}
			at = next
			draw()
			if *result != "" {
				os.WriteFile(*result, []byte(ids[at]), 0o644)
				nudgeSidebar(syscall.SIGUSR2)
			}
		})
	})
}

func termSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 100, 30
	}
	return int(ws.Col), int(ws.Row)
}

// previewMessages bounds how much of the transcript a peek reads.
const previewMessages = 40

// renderPreview lays out a header and the newest messages that fit, bottom
// aligned like a chat.
func renderPreview(s store.Session, w, h int, hold bool, pos string) string {
	w = max(w, 20)
	title := stTitle.Render(ansi.Truncate(label(s), w-len(pos)-1, "…"))
	if pos != "" {
		title += strings.Repeat(" ", max(w-lipgloss.Width(title)-len(pos), 1)) + stSub.Render(pos)
	}
	head := []string{title}
	head = append(head,
		stSub.Render(ansi.Truncate(fmt.Sprintf("%s · %s · %s %s ago",
			collapseHome(s.Cwd), s.Profile, s.Status, ago(s.StatusSince)), w, "…")),
		stLine.Render(strings.Repeat("─", w)),
	)
	foot := ""
	if hold {
		foot = stMuted.Render("↑↓ other sessions · release space to close")
	}

	var body []string
	msgs, err := transcript.Tail(s.TranscriptPath, previewMessages)
	switch {
	case s.TranscriptPath == "" || errors.Is(err, os.ErrNotExist):
		body = []string{stSub.Render("no transcript — the session never exchanged a message")}
	case err != nil:
		body = []string{stErr.Render(err.Error())}
	case len(msgs) == 0:
		body = []string{stSub.Render("no messages yet")}
	}
	wrap := lipgloss.NewStyle().Width(w - 2)
	for _, m := range msgs {
		who, st := stKey.Render("you"), stText
		if m.Role == "assistant" {
			who, st = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C792EA")).Render("claude"), stSub
		}
		body = append(body, "", who)
		for _, line := range strings.Split(wrap.Render(m.Text), "\n") {
			body = append(body, "  "+st.Render(line))
		}
	}

	room := h - len(head)
	if foot != "" {
		room--
	}
	if len(body) > room {
		body = body[len(body)-max(room, 0):]
	}
	lines := append(head, body...)
	if foot != "" {
		for len(lines) < h-1 {
			lines = append(lines, "")
		}
		lines = append(lines, foot)
	}
	return strings.Join(lines, "\r\n")
}

// holdWhileSpace returns once space is let go. Terminals report no key
// releases (and tmux would not pass them on), so the physical key state is
// asked of macOS through the harness-keywait helper: once it confirms the
// key is down, its exit is the release, within ~10 ms. Without it (helper
// missing, or a tap already over) "held" falls back to "the next
// auto-repeat arrived in time": the first repeat comes after macOS's
// InitialKeyRepeat delay, later ones every KeyRepeat. Any other key closes.
func holdWhileSpace(move func(delta int)) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)

	keys := make(chan string)
	go func() {
		b := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil {
				close(keys)
				return
			}
			for _, k := range parseKeys(b[:n]) {
				keys <- k
			}
		}
	}()

	down, released, stop := watchSpace()
	defer stop()

	initial, repeat := keyRepeat()
	wait := initial + 400*time.Millisecond // until the first repeat (generous: a gap here would close and reopen)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	confirmed := false // macOS says the key is held: only its release counts
	for {
		select {
		case <-down:
			confirmed, down = true, nil
			timer.Stop()
		case <-released:
			return nil
		case k, ok := <-keys:
			switch {
			case !ok:
				return nil
			case k == "up" || k == "down":
				move(map[string]int{"up": -1, "down": 1}[k])
			case k != " ":
				return nil
			}
			// Pressing an arrow ends space's auto-repeat; with only repeat
			// timing to go on, the arrow itself counts as holding on.
			if !confirmed {
				timer.Reset(max(3*repeat, 150*time.Millisecond))
			}
		case <-timer.C:
			return nil
		}
	}
}

// parseKeys turns raw terminal input into " ", "up", "down" or "other".
func parseKeys(b []byte) []string {
	var keys []string
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O'):
			switch b[i+2] {
			case 'A':
				keys = append(keys, "up")
			case 'B':
				keys = append(keys, "down")
			default:
				keys = append(keys, "other")
			}
			i += 2
		case c == ' ':
			keys = append(keys, " ")
		case c == 'k':
			keys = append(keys, "up")
		case c == 'j':
			keys = append(keys, "down")
		default:
			keys = append(keys, "other")
		}
	}
	return keys
}

// watchSpace runs harness-keywait next to the harness binary. down fires
// when it sees space held, released when it is let go; both stay silent if
// the helper is missing or cannot tell.
func watchSpace() (down, released <-chan struct{}, stop func()) {
	d, r := make(chan struct{}), make(chan struct{})
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, func() {}
	}
	cmd := exec.Command(filepath.Join(filepath.Dir(exe), "harness-keywait"), "49")
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return nil, nil, func() {}
	}
	go func() {
		buf := make([]byte, 16)
		if n, _ := out.Read(buf); strings.HasPrefix(string(buf[:n]), "down") {
			close(d)
			if cmd.Wait() == nil {
				close(r)
			}
			return
		}
		cmd.Wait() // not held (exit 2): leave it to key repeat timing
	}()
	return d, r, func() { cmd.Process.Kill() }
}

// keyRepeat reads the macOS keyboard repeat settings (units of 15 ms),
// falling back to the system defaults.
func keyRepeat() (initial, repeat time.Duration) {
	read := func(key string, def int) time.Duration {
		out, err := exec.Command("defaults", "read", "-g", key).Output()
		n, perr := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || perr != nil || n <= 0 {
			n = def
		}
		return time.Duration(n) * 15 * time.Millisecond
	}
	return read("InitialKeyRepeat", 25), read("KeyRepeat", 6)
}

// previewCommand is what the sidebar runs in the popup: peek at id, with
// ↑/↓ walking ids and the final choice written to result.
func previewCommand(id string, ids []string, result string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "harness"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	return fmt.Sprintf("%s preview --hold --ids %s --result %s %s", q(exe), q(strings.Join(ids, ",")), q(result), q(id))
}
