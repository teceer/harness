// Package tmux is a thin wrapper over the tmux CLI. tmux hosts the claude
// processes, so the harness itself can crash or restart without harm.
//
// Harness runs its own tmux server (socket "harness", config written by
// WriteConf) so it never touches, or collides with, the user's own tmux.
package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Socket names the harness tmux server; HARNESS_TMUX_SOCKET overrides it
// (tests run against a throwaway server).
var Socket = socketName()

func socketName() string {
	if s := os.Getenv("HARNESS_TMUX_SOCKET"); s != "" {
		return s
	}
	return "harness"
}

// ConfPath is passed as -f; it only matters when a call starts the server.
var ConfPath string

func argv(args ...string) []string {
	a := []string{"-L", Socket}
	if ConfPath != "" {
		a = append(a, "-f", ConfPath)
	}
	return append(a, args...)
}

func run(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("tmux", argv(args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// OwnsTMUXEnv reports whether a $TMUX value points at the harness server.
// Pane ids are per server, so panes of the user's own tmux must be ignored.
func OwnsTMUXEnv(tmuxEnv string) bool {
	sock, _, _ := strings.Cut(tmuxEnv, ",")
	return sock != "" && filepath.Base(sock) == Socket
}

func Inside() bool { return OwnsTMUXEnv(os.Getenv("TMUX")) }

// Panes maps every live pane id (e.g. "%12") to its process pid; panes
// kept around by remain-on-exit are skipped. No tmux server means no panes.
func Panes() map[string]int {
	out, err := run("list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_dead}")
	panes := map[string]int{}
	if err != nil {
		return panes
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] == "1" {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		panes[f[0]] = n
	}
	return panes
}

func hasSession(name string) bool {
	return exec.Command("tmux", argv("has-session", "-t", "="+name)...).Run() == nil
}

// NewWindow starts command in a new background window of session (created
// on demand) and returns the new pane id.
func NewWindow(session, name, cwd string, env map[string]string, command string) (string, error) {
	args := []string{}
	if hasSession(session) {
		args = append(args, "new-window", "-d", "-t", "="+session+":")
	} else {
		args = append(args, "new-session", "-d", "-s", session, "-x", "200", "-y", "50")
	}
	args = append(args, "-P", "-F", "#{pane_id}", "-n", name, "-c", cwd)
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, command)
	return run(args...)
}

// FitToSlot sizes pane's (background) window like the slot next to the
// sidebar, so the first Show does not resize, and reflow, the session.
func FitToSlot(pane string, sidebarWidth int) {
	sb := Sidebar()
	if sb == "" {
		return
	}
	if _, w, h, err := slot(sb, sidebarWidth); err == nil {
		run("resize-window", "-t", pane, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
	}
}

// slot returns the column and width of the area where sessions show, right
// of the sidebar, and the window height: the shown pane's when there is
// one, otherwise what the sidebar and the changes pane leave free.
func slot(sidebar string, sidebarWidth int) (left, width, height int, err error) {
	infos, err := paneInfos()
	if err != nil {
		return 0, 0, 0, err
	}
	sb, ok := infos[sidebar]
	if !ok {
		return 0, 0, 0, fmt.Errorf("sidebar pane %s is gone", sidebar)
	}
	out, err := run("display-message", "-p", "-t", sidebar, "#{window_width} #{window_height}")
	if err != nil {
		return 0, 0, 0, err
	}
	var ww, wh int
	if _, err := fmt.Sscan(out, &ww, &wh); err != nil {
		return 0, 0, 0, err
	}
	if shown := slotted(infos, sidebar); len(shown) == 1 {
		in := infos[shown[0]]
		return in.left, in.width, wh, nil
	}
	left, width = sidebarWidth+1, ww-sidebarWidth-1
	if p := changesIn(infos, sb.window); p != "" {
		width -= changesWidth(p, infos[p].width) + 1
	}
	if width < 20 {
		return 0, 0, 0, fmt.Errorf("window too narrow (%d columns)", ww)
	}
	return left, width, wh, nil
}

func KillPane(pane string) error {
	_, err := run("kill-pane", "-t", pane)
	return err
}

// Attach focuses pane: select it when already inside the harness server,
// otherwise replace this process with a tmux client attached to it.
func Attach(pane string) error {
	bin, argv, env, err := attachCmd(pane)
	if err != nil || bin == "" {
		return err
	}
	return syscall.Exec(bin, argv, env)
}

// AttachWait is Attach as a child process, so the caller can clean up
// (e.g. restore the terminal profile) after the client detaches.
func AttachWait(pane string) error {
	bin, argv, env, err := attachCmd(pane)
	if err != nil || bin == "" {
		return err
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, env
	return cmd.Run()
}

// attachCmd selects pane and returns the tmux client command to run; an
// empty bin means we are inside the server and switched there already.
func attachCmd(pane string) (string, []string, []string, error) {
	if _, err := run("select-window", "-t", pane); err != nil {
		return "", nil, nil, err
	}
	if _, err := run("select-pane", "-t", pane); err != nil {
		return "", nil, nil, err
	}
	if Inside() {
		_, err := run("switch-client", "-t", pane)
		return "", nil, nil, err
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return "", nil, nil, err
	}
	session, err := run("display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return "", nil, nil, err
	}
	env := os.Environ()
	// Attaching from inside another tmux would nest; make it explicit.
	for i, e := range env {
		if strings.HasPrefix(e, "TMUX=") {
			env[i] = "TMUX="
		}
	}
	return bin, append([]string{"tmux"}, argv("attach-session", "-t", "="+session)...), env, nil
}

// ---- sidebar layout ----

const (
	sidebarOpt = "@harness_sidebar"
	serveOpt   = "@harness_serve"
	changesOpt = "@harness_changes"
	buildOpt   = "@harness_build" // build of the binary the sidebar runs
	// changesWidthOpt keeps the width the changes pane was opened with, to
	// restore it when the layout is rebuilt around it.
	changesWidthOpt = "@harness_changes_width"
)

func paneOption(pane, opt string) string {
	out, _ := run("display-message", "-p", "-t", pane, "#{"+opt+"}")
	return out
}

const pidOpt = "@harness_sidebar_pid"

// MarkPID records the sidebar process, which reloads on SIGUSR1.
func MarkPID(pane string, pid int) {
	run("set-option", "-p", "-t", pane, pidOpt, strconv.Itoa(pid))
}

// SidebarPID is the sidebar process in pane, 0 if unknown.
func SidebarPID(pane string) int {
	n, _ := strconv.Atoi(paneOption(pane, pidOpt))
	return n
}

// MarkBuild records which harness build runs in pane (the sidebar calls it
// at start, so `harness ui` can tell a stale sidebar).
func MarkBuild(pane, build string) {
	run("set-option", "-p", "-t", pane, buildOpt, build)
}

// Sidebar returns the pane running the sidebar, "" if there is none.
func Sidebar() string { return paneWith(sidebarOpt) }

// paneWith returns the live pane marked with opt, "" if there is none.
func paneWith(opt string) string {
	out, err := run("list-panes", "-a", "-F", "#{pane_id} #{"+opt+"} #{pane_dead}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[1] == "1" && f[2] == "0" {
			return f[0]
		}
	}
	return ""
}

// EnsureSidebar starts the sidebar (command) in window "main" unless it
// runs, restarts it in place when it runs an older build than build, and
// (re)applies the key bindings either way; exe is the harness binary the
// switch bindings call.
func EnsureSidebar(session, command, exe, build string) (string, error) {
	pane := Sidebar()
	if pane != "" && paneOption(pane, buildOpt) != build {
		// Same pane id (bindings stay valid), fresh process; sessions untouched.
		if _, err := run("respawn-pane", "-k", "-t", pane, command); err != nil {
			return "", err
		}
	}
	if pane == "" {
		args := []string{"new-session", "-s", session, "-x", "200", "-y", "50"}
		if hasSession(session) {
			args = []string{"new-window", "-t", "=" + session + ":"}
		}
		args = append(args, "-d", "-P", "-F", "#{pane_id}", "-n", "main", command)
		var err error
		if pane, err = run(args...); err != nil {
			return "", err
		}
		if _, err := run("set-option", "-p", "-t", pane, sidebarOpt, "1"); err != nil {
			return "", err
		}
	}
	return pane, bindKeys(pane, exe)
}

// EnsureServe keeps the remote control (command) running in a background
// window "serve" of session, next to the sidebar, and restarts it when it
// runs an older build. The window list is hidden, so it never shows up.
func EnsureServe(session, command, build string) error {
	pane := paneWith(serveOpt)
	if pane != "" {
		if paneOption(pane, buildOpt) == build {
			return nil
		}
		_, err := run("respawn-pane", "-k", "-t", pane, command)
		if err == nil {
			run("set-option", "-p", "-t", pane, buildOpt, build)
		}
		return err
	}
	pane, err := NewWindow(session, "serve", os.Getenv("HOME"), nil, command)
	if err != nil {
		return err
	}
	_, err = run("set-option", "-p", "-t", pane, serveOpt, "1", ";",
		"set-option", "-p", "-t", pane, buildOpt, build)
	return err
}

// Key sequences the harness iTerm2 profile sends for ⌘[ ⌘] ⌥⇥ ⌘⇧A ⌘⇧N ⌘⇧G and ⌥1…⌥9.
// Cmd chords never reach a terminal program on their own; these private CSI
// sequences are what iTerm2 is told to send instead (see internal/iterm).
const (
	SeqPrev     = "\x1b[1000~"
	SeqNext     = "\x1b[1001~"
	SeqToggle   = "\x1b[1002~"
	SeqArchived = "\x1b[1003~"
	SeqNew      = "\x1b[1004~"
	SeqChanges  = "\x1b[1005~"
)

// Keys the sidebar receives for ⌘⇧A: pressed in the sidebar it toggles
// Sessions ⇄ Archived; pressed in a session it jumps to Archived.
const (
	TabToggleKey   = "F12"
	TabArchivedKey = "F11"
	// NewSessionKey opens the sidebar's new-session prompt (⌘⇧N).
	NewSessionKey = "F10"
)

// SeqNumber is the sequence for ⌥n, n in 1…9.
func SeqNumber(n int) string { return fmt.Sprintf("\x1b[10%d~", 10+n) }

// bindKeys: ⌥⇥ cycles the sidebar, the session next to it and the changes pane,
// ⌘[ / ⌘] show the previous / next harness session, ⌥n the n-th one,
// ⌘⇧A toggles Sessions ⇄ Archived, ⌘⇧N starts a new session, ⌘⇧G
// opens / closes the changes pane.
func bindKeys(sidebar, exe string) error {
	seqs := []string{SeqPrev, SeqNext, SeqToggle}
	back := fmt.Sprintf("select-window -t %s ; select-pane -t %s", sidebar, sidebar)
	binds := [][]string{
		{"User0", "run-shell", "-b", Quote(exe) + " switch prev"},
		{"User1", "run-shell", "-b", Quote(exe) + " switch next"},
		// sidebar → session → changes pane (when open) → sidebar: -R wraps
		// around from the rightmost pane; any other window goes back.
		{"User2", "if-shell", "-F", "#{||:#{" + changesOpt + "},#{==:#{window_panes},1}}", back, "select-pane -R"},
	}
	for n := 1; n <= 9; n++ {
		seqs = append(seqs, SeqNumber(n))
		binds = append(binds, []string{fmt.Sprintf("User%d", len(seqs)-1), "run-shell", "-b", fmt.Sprintf("%s switch %d", Quote(exe), n)})
	}
	// ⌘⇧N: focus the sidebar and open its new-session prompt.
	seqs = append(seqs, SeqNew)
	binds = append(binds, []string{fmt.Sprintf("User%d", len(seqs)-1),
		"select-window", "-t", sidebar, `\;`, "select-pane", "-t", sidebar, `\;`, "send-keys", "-t", sidebar, NewSessionKey})

	// ⌘⇧A: in the sidebar toggle its tabs; from a session focus the
	// sidebar on Archived.
	seqs = append(seqs, SeqArchived)
	binds = append(binds, []string{fmt.Sprintf("User%d", len(seqs)-1),
		"if-shell", "-F", "#{" + sidebarOpt + "}",
		"send-keys -t " + sidebar + " " + TabToggleKey,
		fmt.Sprintf("select-window -t %s ; select-pane -t %s ; send-keys -t %s %s", sidebar, sidebar, sidebar, TabArchivedKey)})

	// ⌘⇧G: open / close the git changes pane.
	seqs = append(seqs, SeqChanges)
	binds = append(binds, []string{fmt.Sprintf("User%d", len(seqs)-1), "run-shell", "-b", Quote(exe) + " changes toggle"})

	// bindings of earlier versions; missing ones are fine
	run("unbind-key", "-n", "M-s")
	run("unbind-key", "-n", `C-\`)

	var cmds [][]string
	for i, seq := range seqs {
		cmds = append(cmds, []string{"set-option", "-s", fmt.Sprintf("user-keys[%d]", i), seq})
	}
	for _, b := range binds {
		cmds = append(cmds, append([]string{"bind-key", "-n"}, b...))
	}
	return batch(cmds...)
}

// Reload re-reads the harness tmux.conf in a running server (no-op if none).
func Reload() {
	if ConfPath != "" {
		run("source-file", ConfPath)
	}
}

// SendText types text into pane and submits it (as the remote control
// answers a Claude prompt). -l keeps it literal: no key names, no escapes.
func SendText(pane, text string) error {
	if _, err := run("send-keys", "-t", pane, "-l", text); err != nil {
		return err
	}
	_, err := run("send-keys", "-t", pane, "Enter")
	return err
}

// Capture returns what a pane shows, colours included, for the remote
// live view. -J rejoins wrapped lines, -e keeps the SGR sequences.
func Capture(pane string) (string, error) {
	return run("capture-pane", "-e", "-p", "-J", "-t", pane)
}

// Scrollback returns up to lines of history above the visible screen (not
// the screen itself), for scrolling back on a phone without touching the
// pane's own copy mode. alt reports a full-screen program (alternate
// screen), which keeps no scrollback in tmux at all.
func Scrollback(pane string, lines int) (out string, alt bool, err error) {
	info, err := run("display-message", "-p", "-t", pane, "#{alternate_on} #{history_size}")
	if err != nil {
		return "", false, err
	}
	var size int
	var altOn string
	fmt.Sscan(info, &altOn, &size)
	if altOn == "1" || size == 0 || lines <= 0 {
		return "", altOn == "1", nil
	}
	out, err = run("capture-pane", "-e", "-p", "-J", "-t", pane, "-S", "-"+strconv.Itoa(min(lines, size)), "-E", "-1")
	return out, false, err
}

// Wheel scrolls a full-screen program the way a mouse wheel over the pane
// does: lines < 0 scrolls up (back), > 0 down. It only applies to a pane
// on the alternate screen whose program asked for SGR mouse reports (as
// Claude Code does); ok is false otherwise.
func Wheel(pane string, lines int) (ok bool, err error) {
	info, err := run("display-message", "-p", "-t", pane, "#{alternate_on} #{mouse_sgr_flag} #{pane_width} #{pane_height}")
	if err != nil {
		return false, err
	}
	var alt, sgr string
	var w, h int
	fmt.Sscan(info, &alt, &sgr, &w, &h)
	if alt != "1" || sgr != "1" {
		return false, nil
	}
	button, n := 65, lines // 64 wheel up, 65 wheel down
	if lines < 0 {
		button, n = 64, -lines
	}
	ev := fmt.Sprintf("\x1b[<%d;%d;%dM", button, max(1, w/2), max(1, h/2))
	for range n {
		if _, err := run("send-keys", "-t", pane, "-l", ev); err != nil {
			return true, err
		}
	}
	return true, nil
}

// SendKey presses one named key in pane (Enter, Escape, 1, y, …).
func SendKey(pane, key string) error {
	_, err := run("send-keys", "-t", pane, key)
	return err
}

// Focus moves the keyboard to pane.
func Focus(pane string) error {
	_, err := run("select-pane", "-t", pane)
	return err
}

type paneInfo struct {
	window              string
	left, width, height int
	changes             bool // the git changes pane
}

func paneInfos() (map[string]paneInfo, error) {
	out, err := run("list-panes", "-a", "-F",
		"#{pane_id} #{window_id} #{pane_left} #{pane_width} #{pane_height} #{?#{"+changesOpt+"},1,0}")
	if err != nil {
		return nil, err
	}
	infos := map[string]paneInfo{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 6 {
			continue
		}
		l, _ := strconv.Atoi(f[2])
		w, _ := strconv.Atoi(f[3])
		h, _ := strconv.Atoi(f[4])
		infos[f[0]] = paneInfo{window: f[1], left: l, width: w, height: h, changes: f[5] == "1"}
	}
	return infos, nil
}

// slotted returns the session panes in the sidebar's window: everything
// there but the sidebar and the changes pane (normally exactly one).
func slotted(infos map[string]paneInfo, sidebar string) []string {
	var shown []string
	for p, in := range infos {
		if in.window == infos[sidebar].window && p != sidebar && !in.changes {
			shown = append(shown, p)
		}
	}
	return shown
}

// changesIn returns the changes pane of window, "" if it is closed.
func changesIn(infos map[string]paneInfo, window string) string {
	for p, in := range infos {
		if in.window == window && in.changes {
			return p
		}
	}
	return ""
}

func changesWidth(pane string, fallback int) int {
	if n, err := strconv.Atoi(paneOption(pane, changesWidthOpt)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// batch runs several tmux commands in one invocation: the server applies
// them together and redraws once, so no intermediate layout ever shows.
func batch(cmds ...[]string) error {
	var args []string
	for i, c := range cmds {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, c...)
	}
	if len(args) == 0 {
		return nil
	}
	_, err := run(args...)
	return err
}

// Show places target to the right of the sidebar and focuses it when focus
// is set. The pane shown so far is swapped into target's old window, which
// is sized like the slot: neither claude sees a resize, so nothing reflows
// or flickers. Everything happens in a single tmux call.
func Show(sidebar, target string, width int, nameOf func(pane string) string, focus bool) error {
	infos, err := paneInfos()
	if err != nil {
		return err
	}
	sb, ok := infos[sidebar]
	if !ok {
		return fmt.Errorf("sidebar pane %s is gone", sidebar)
	}
	tg, ok := infos[target]
	if !ok {
		return fmt.Errorf("pane %s is gone", target)
	}
	shown := slotted(infos, sidebar)

	var cmds [][]string
	switch {
	case tg.window == sb.window: // already on the right
	case len(shown) == 1:
		cur := infos[shown[0]]
		cmds = append(cmds,
			[]string{"swap-pane", "-d", "-s", target, "-t", shown[0]},
			// shown[0] now lives in target's old window: keep it slot-sized.
			[]string{"resize-window", "-t", tg.window, "-x", strconv.Itoa(cur.width), "-y", strconv.Itoa(cur.height)},
			[]string{"rename-window", "-t", tg.window, windowLabel(nameOf(shown[0]))},
		)
	default: // nothing shown yet (or a stray layout): rebuild the pair
		for _, p := range shown {
			cmds = append(cmds, []string{"break-pane", "-d", "-s", p, "-n", windowLabel(nameOf(p))})
		}
		if ch := changesIn(infos, sb.window); ch != "" {
			// Slot in left of the changes pane, then restore both widths.
			cmds = append(cmds,
				[]string{"join-pane", "-d", "-h", "-b", "-s", target, "-t", ch},
				[]string{"resize-pane", "-t", sidebar, "-x", strconv.Itoa(width)},
				[]string{"resize-pane", "-t", ch, "-x", strconv.Itoa(changesWidth(ch, infos[ch].width))},
			)
		} else {
			cmds = append(cmds,
				[]string{"join-pane", "-d", "-h", "-s", target, "-t", sidebar},
				[]string{"resize-pane", "-t", sidebar, "-x", strconv.Itoa(width)},
			)
		}
	}
	if focus {
		cmds = append(cmds, []string{"select-pane", "-t", target})
	}
	return batch(cmds...)
}

func windowLabel(s string) string {
	if s == "" {
		return "claude"
	}
	if r := []rune(s); len(r) > 24 {
		return string(r[:24])
	}
	return s
}

// Shown returns the pane displayed next to the sidebar, "" if none.
func Shown(sidebar string) string {
	out, err := run("list-panes", "-t", sidebar, "-F", "#{pane_id} #{?#{"+changesOpt+"},1,0}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] != sidebar && f[1] == "0" {
			return f[0]
		}
	}
	return ""
}

// Changes returns the git changes pane, "" when it is closed.
func Changes() string { return paneWith(changesOpt) }

// ToggleChanges opens the git changes pane (running command) width columns
// wide at the right edge of the sidebar's window, or closes it. The shown
// session reflows once; every background window is then fitted to the new
// slot, so switching sessions still never resizes one.
func ToggleChanges(sidebar string, sidebarWidth, width int, command string) (open bool, err error) {
	if p := Changes(); p != "" {
		if err := KillPane(p); err != nil {
			return true, err
		}
		return false, fitBackground(sidebar, sidebarWidth)
	}
	target := Shown(sidebar)
	if target == "" {
		target = sidebar
	}
	pane, err := run("split-window", "-h", "-d", "-l", strconv.Itoa(width), "-t", target, "-P", "-F", "#{pane_id}", command)
	if err != nil {
		return false, err
	}
	err = batch(
		[]string{"set-option", "-p", "-t", pane, changesOpt, "1"},
		[]string{"set-option", "-p", "-t", pane, changesWidthOpt, strconv.Itoa(width)},
	)
	if err != nil {
		return true, err
	}
	return true, fitBackground(sidebar, sidebarWidth)
}

// RespawnChanges restarts the changes pane in place when it runs an older
// build than build (it marks its build itself, like the sidebar).
func RespawnChanges(command, build string) error {
	pane := Changes()
	if pane == "" || paneOption(pane, buildOpt) == build {
		return nil
	}
	_, err := run("respawn-pane", "-k", "-t", pane, command)
	return err
}

// fitBackground sizes every other window of the sidebar's session like the
// slot, as FitToSlot does for a new one.
func fitBackground(sidebar string, sidebarWidth int) error {
	_, w, h, err := slot(sidebar, sidebarWidth)
	if err != nil {
		return err
	}
	out, err := run("display-message", "-p", "-t", sidebar, "#{session_id} #{window_id}")
	if err != nil {
		return err
	}
	var session, own string
	fmt.Sscan(out, &session, &own)
	out, err = run("list-windows", "-t", session, "-F", "#{window_id}")
	if err != nil {
		return err
	}
	var cmds [][]string
	for _, win := range strings.Fields(out) {
		if win != own {
			cmds = append(cmds, []string{"resize-window", "-t", win, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h)})
		}
	}
	return batch(cmds...)
}

// Popup runs command in a bordered popup laid exactly over the slot right
// of the sidebar (or where it would be), and returns when it closes. The
// popup takes the keyboard while it is open.
func Popup(sidebar string, sidebarWidth int, title, command string) error {
	left, width, wh, err := slot(sidebar, sidebarWidth)
	if err != nil || width < 20 {
		out, derr := run("display-message", "-p", "-t", sidebar, "#{window_width} #{window_height}")
		if derr != nil {
			return derr
		}
		fmt.Sscan(out, &width, &wh)
		left = 0
	}
	cmd := exec.Command("tmux", argv("display-popup", "-E", "-T", " "+title+" ", "-S", "fg=#7AA2FF", "-t", sidebar,
		"-x", strconv.Itoa(left), "-y", strconv.Itoa(wh), "-w", strconv.Itoa(width), "-h", strconv.Itoa(wh), command)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	// display-popup exits with the command's own status: a pager that is
	// quit, or 129 (SIGHUP) when tmux closes the popup under it. Only a
	// failure tmux reports itself is an error.
	if _, ok := err.(*exec.ExitError); ok && strings.TrimSpace(stderr.String()) == "" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tmux display-popup: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Message shows text on the status line of the harness clients.
func Message(text string) {
	run("display-message", text)
}

func DetachClient() error {
	_, err := run("detach-client")
	return err
}

// Quote makes s safe as a single word for the shell tmux runs commands with.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteConf writes the harness server config. User additions go to
// tmux.local.conf next to it, which is sourced when present.
func WriteConf(path string) error {
	local := filepath.Join(filepath.Dir(path), "tmux.local.conf")
	conf := `# generated by harness — edit tmux.local.conf instead
set -g default-terminal "tmux-256color"
set -as terminal-features ",xterm-256color:RGB,xterm*:extkeys"
set -g extended-keys on
set-environment -g COLORTERM truecolor
set -g escape-time 0
set -g focus-events on
set -g history-limit 50000
set -g mouse on
set -g allow-passthrough on
set -g set-clipboard on
set -g status-style "bg=default,fg=#7C849C"
set -g status-left ""
set -g window-status-format ""
set -g window-status-current-format ""
set -g status-right " ⌥⇥ next pane · ⌘[ ⌘] ⌥1-9 switch · ⌘⇧G changes · C-b d detach "
set -g pane-border-style "fg=#3A4160"
set -g pane-active-border-style "fg=#7AA2FF"
if-shell "test -f ` + Quote(local) + `" "source-file ` + Quote(local) + `"
`
	if cur, err := os.ReadFile(path); err == nil && string(cur) == conf {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(conf), 0o644)
}
