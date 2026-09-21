package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/iterm"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// cmdUI makes sure the sidebar runs in the harness tmux server and attaches
// to it. The sidebar is restarted by its shell loop if it ever crashes;
// exiting it cleanly (ctrl+c) ends the loop.
func cmdUI(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	loop := fmt.Sprintf("while :; do %s sidebar && break; sleep 1; done", tmux.Quote(exe))
	tmux.Reload() // pick up tmux.conf changes in an already running server
	pane, err := tmux.EnsureSidebar(cfg.TmuxSession, loop, exe, buildID())
	if err != nil {
		return err
	}
	if !iterm.Active() {
		return tmux.Attach(pane)
	}
	// ⌘[ ⌘] ⌥⇥ only exist in the Harness profile: wear it while attached.
	orig := iterm.CurrentProfile()
	if err := iterm.EnsureProfile(orig); err != nil {
		fmt.Fprintln(os.Stderr, "harness: iTerm2 profile:", err)
		return tmux.Attach(pane)
	}
	iterm.SetProfile(os.Stdout, iterm.ProfileName)
	defer iterm.SetProfile(os.Stdout, orig)
	return tmux.AttachWait(pane)
}

// buildID identifies the running binary (size and mtime), so a sidebar
// started from an older build can be told apart and restarted.
func buildID() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}

// nudgeSidebar signals the sidebar process. SIGUSR1/2 terminate processes
// that do not handle them: only signal the pid while it still is our
// sidebar, never whatever reused the number.
func nudgeSidebar(sig syscall.Signal) {
	sb := tmux.Sidebar()
	if sb == "" {
		return
	}
	if pid := tmux.SidebarPID(sb); pid > 0 && isSidebarProcess(pid) {
		syscall.Kill(pid, sig)
	}
}

func isSidebarProcess(pid int) bool {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	f := strings.Fields(string(out))
	return err == nil && len(f) >= 2 && filepath.Base(f[0]) == "harness" && f[1] == "sidebar"
}

// bumpFile is touched by commands that change the layout behind the
// sidebar's back, so it refreshes at once (see sidebar.stamp).
const bumpFile = "ui.bump"

func bump(cfg *config.Config) {
	os.WriteFile(filepath.Join(cfg.Home, bumpFile), []byte(time.Now().Format(time.RFC3339Nano)), 0o644)
}

// cmdSwitch shows the previous/next or the n-th harness session next to the
// sidebar (⌘[ ⌘] ⌥1…⌥9). It runs from a tmux key binding, where an exit
// status only surfaces as "returned 1": problems are shown on the tmux
// status line instead.
func cmdSwitch(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: harness switch prev|next|1…9")
	}
	err := withStore(func(cfg *config.Config, st *store.Store) error {
		return switchTo(cfg, st, args[0])
	})
	if err != nil && tmux.Sidebar() != "" {
		tmux.Message("harness: " + err.Error())
		return nil
	}
	return err
}

func switchTo(cfg *config.Config, st *store.Store, which string) error {
	all, err := st.List()
	if err != nil {
		return err
	}
	sb := tmux.Sidebar()
	if sb == "" {
		return fmt.Errorf("sidebar is not running")
	}
	// Only sessions whose pane really exists: a stale row must never make
	// the shortcut fail. The sidebar numbers the same list (after sync).
	live := tmux.Panes()
	var order []store.Session
	for _, s := range switchOrder(all, time.Now()) {
		if _, ok := live[s.TmuxPane]; ok {
			order = append(order, s)
		}
	}
	var target store.Session
	switch which {
	case "prev", "next":
		var ok bool
		if target, ok = neighbour(order, tmux.Shown(sb), map[string]int{"prev": -1, "next": 1}[which]); !ok {
			return nil
		}
	default:
		n, err := strconv.Atoi(which)
		if err != nil || n < 1 {
			return fmt.Errorf("usage: harness switch prev|next|1…9")
		}
		if n > len(order) {
			return nil // no such session: ignore the key
		}
		target = order[n-1]
	}
	names := map[string]string{}
	for _, s := range all {
		if s.TmuxPane != "" {
			names[s.TmuxPane] = windowName(s)
		}
	}
	err = tmux.Show(sb, target.TmuxPane, cfg.SidebarWidth, func(p string) string { return names[p] }, true)
	nudgeSidebar(syscall.SIGUSR1)
	bump(cfg) // fallback for a sidebar that cannot be signalled
	return err
}

// uiDebug logs every non-periodic UI message when HARNESS_UI_DEBUG names a file.
var uiDebug *os.File

func cmdSidebar(args []string) error {
	if p := os.Getenv("HARNESS_UI_DEBUG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			uiDebug = f
			defer f.Close()
		}
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		self := ""
		if tmux.Inside() {
			self = os.Getenv("TMUX_PANE")
			tmux.MarkBuild(self, buildID())
			tmux.MarkPID(self, os.Getpid())
			// Our tmux advertises RGB and downsamples for the outer terminal
			// itself, so detection (which sees only TERM=tmux-256color) is moot.
			lipgloss.SetColorProfile(termenv.TrueColor)
		}
		m := newSidebar(cfg, st, self)
		// Focus reports tell the sidebar when the keyboard moves into the
		// session next to it (tmux runs with focus-events on).
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus())
		// SIGUSR1 = "state changed, reload now" (sent by `harness switch`),
		// SIGUSR2 = "the peek moved to another session" (preview's ↑/↓).
		nudge := make(chan os.Signal, 4)
		signal.Notify(nudge, syscall.SIGUSR1, syscall.SIGUSR2)
		defer signal.Stop(nudge)
		go func() {
			for sig := range nudge {
				if sig == syscall.SIGUSR2 {
					p.Send(peekMovedMsg{})
				} else {
					p.Send(nudgeMsg{})
				}
			}
		}()
		_, err := p.Run()
		return err
	})
}

// ---- model ----

type inputMode int

const (
	modeNormal inputMode = iota
	modeNew
	modeRename
)

type sidebar struct {
	cfg  *config.Config
	st   *store.Store
	self string // our own tmux pane; "" when not run by `harness ui`

	sessions    []store.Session
	shown       string // pane currently displayed next to the sidebar
	selID       string
	selPane     string // pane to select once it shows up in the data
	lastPane    string // pane of the selected session, to follow id changes
	offset      int
	tab         tab
	selByTab    [len(tabNames)]string // selection remembered per tab
	tabHits     [len(tabNames)][2]int // header columns of each tab, for clicks
	showEnded   bool
	lastSync    time.Time
	lastLoad    time.Time
	lastStamp   string
	loading     bool
	compl       completer
	pendingA    string // id awaiting a second `a` to force-archive
	confirmMove string // id of a terminal-tab session awaiting Y/n
	peeking     bool   // the hold-space preview popup is open

	width, height int
	msg           string
	msgErr        bool
	msgAt         time.Time

	mode  inputMode
	input textinput.Model
}

func newSidebar(cfg *config.Config, st *store.Store, self string) *sidebar {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 512
	return &sidebar{cfg: cfg, st: st, self: self, input: in, width: 40, height: 20}
}

type tickMsg time.Time

// nudgeMsg asks for an immediate reload (SIGUSR1 from `harness switch`).
type nudgeMsg struct{}

type dataMsg struct {
	sessions []store.Session
	shown    string
	synced   bool
	err      error
}

// askMoveMsg: the session to open lives in a plain terminal tab.
type askMoveMsg struct{ id string }

type opMsg struct {
	text       string
	err        error
	selectPane string // pane to select after the op
}

const (
	pollEvery    = 100 * time.Millisecond // cheap stat of the state files
	refreshEvery = 2 * time.Second        // reload anyway, for the ages
	syncEvery    = 5 * time.Second        // ps + tmux liveness check
)

func tick() tea.Cmd {
	return tea.Tick(pollEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *sidebar) Init() tea.Cmd {
	m.loading = true
	return tea.Batch(m.load(true), tick())
}

// stamp fingerprints everything that signals new state: the database (hooks
// write it) and ui.bump (touched by `harness switch`). A change reloads the
// sidebar within one poll instead of waiting for the next refresh.
func (m *sidebar) stamp() string {
	var b strings.Builder
	for _, name := range []string{"state.db", "state.db-wal", bumpFile} {
		if fi, err := os.Stat(filepath.Join(m.cfg.Home, name)); err == nil {
			fmt.Fprintf(&b, "%d/%d;", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return b.String()
}

// load reads state off the UI goroutine; sync (ps + tmux) only every few seconds.
func (m *sidebar) load(doSync bool) tea.Cmd {
	st, self := m.st, m.self
	return func() tea.Msg {
		if doSync {
			if err := sync(st); err != nil {
				return dataMsg{err: err}
			}
		}
		ss, err := st.List()
		shown := ""
		if self != "" {
			shown = tmux.Shown(self)
		}
		return dataMsg{sessions: ss, shown: shown, synced: doSync, err: err}
	}
}

// ---- update ----

func (m *sidebar) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if uiDebug != nil {
		switch msg.(type) {
		case tickMsg, dataMsg:
		default:
			fmt.Fprintf(uiDebug, "%s %T %q\n", time.Now().Format("15:04:05.000"), msg, fmt.Sprint(msg))
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case nudgeMsg:
		if m.loading {
			m.lastStamp = "" // reload again as soon as the current load ends
			return m, nil
		}
		m.loading, m.lastStamp = true, m.stamp()
		return m, m.load(false)
	case tickMsg:
		stamp := m.stamp()
		if m.loading || (stamp == m.lastStamp && time.Since(m.lastLoad) < refreshEvery) {
			return m, tick()
		}
		m.loading, m.lastStamp = true, stamp
		return m, tea.Batch(m.load(time.Since(m.lastSync) >= syncEvery), tick())
	case dataMsg:
		m.loading, m.lastLoad = false, time.Now()
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
			return m, nil
		}
		if msg.synced {
			m.lastSync = time.Now()
		}
		// The shown session changed outside the sidebar (⌘[ / ⌘]): follow it.
		if msg.shown != "" && msg.shown != m.shown && m.selPane == "" {
			m.setTab(tabSessions)
			m.selPane = msg.shown
		}
		m.sessions, m.shown = msg.sessions, msg.shown
		m.fixSelection()
		return m, nil
	case opMsg:
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
		} else if msg.text != "" {
			m.flash(msg.text, false)
		}
		if msg.selectPane != "" { // a session was started, resumed or moved
			m.setTab(tabSessions)
			m.selPane, m.selID = msg.selectPane, ""
		}
		return m, m.load(true)
	case peekMovedMsg:
		m.followPeek()
		return m, nil
	case peekDoneMsg:
		m.peeking = false
		m.followPeek()
		os.Remove(m.peekFile())
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
		}
		return m, nil
	case tea.BlurMsg:
		m.selectShown()
		return m, nil
	case askMoveMsg:
		m.confirmMove = msg.id
		return m, nil
	case tea.MouseMsg:
		return m.mouse(msg)
	case tea.KeyMsg:
		if m.mode != modeNormal {
			return m.inputKey(msg)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m *sidebar) flash(text string, isErr bool) {
	m.msg, m.msgErr, m.msgAt = text, isErr, time.Now()
}

func (m *sidebar) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if id := m.confirmMove; id != "" {
		m.confirmMove = ""
		// Yes is the default (⏎); any other key keeps the session where it is.
		switch k.String() {
		case "enter", "y", "Y":
			return m, m.moveHere(id)
		}
		m.flash("not moved", false)
		return m, nil
	}
	if k.String() != "a" {
		m.pendingA = ""
	}
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		if m.self == "" {
			return m, tea.Quit
		}
		return m, func() tea.Msg { return opMsg{err: tmux.DetachClient()} }
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home", "g":
		m.moveTo(0)
	case "end", "G":
		m.moveTo(len(m.visible()) - 1)
	case "left", "h":
		m.setTab(tabSessions)
	case "right", "l", strings.ToLower(tmux.TabArchivedKey): // F11: ⌘⇧A from a session
		m.setTab(tabArchived)
	case strings.ToLower(tmux.TabToggleKey): // F12: ⌘⇧A in the sidebar
		m.setTab(1 - m.tab)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return m, m.jump(int(k.String()[0] - '0'))
	case ".":
		m.showEnded = !m.showEnded
		m.fixSelection()
	case "enter":
		return m, m.open(true)
	case " ": // hold to peek at the conversation, in either tab
		if sel, ok := m.selected(); ok {
			return m, m.peek(sel)
		}
	case "tab":
		// Into the session already on the right; otherwise open the selected one.
		if shown := m.shown; shown != "" {
			return m, func() tea.Msg { return opMsg{err: tmux.Focus(shown)} }
		}
		return m, m.open(true)
	case "r":
		return m, m.open(false)
	case "a", "A":
		s, ok := m.selected()
		if !ok {
			return m, nil
		}
		if !s.Live() {
			return m, nil
		}
		force := k.String() == "A" || m.pendingA == s.ID
		if !force && s.Status == store.Working {
			m.pendingA = s.ID
			m.flash("working — press a again to archive anyway", true)
			return m, nil
		}
		m.pendingA = ""
		st := m.st
		return m, func() tea.Msg {
			if err := archive(st, s, force); err != nil {
				return opMsg{err: err}
			}
			return opMsg{text: "archived " + label(s)}
		}
	case "x", "delete":
		s, ok := m.selected()
		if !ok {
			return m, nil
		}
		if s.Live() {
			m.flash("archive it first (a)", true)
			return m, nil
		}
		st := m.st
		return m, func() tea.Msg {
			err := st.Tx(func(tx *store.Tx) error { return tx.Delete(s.ID) })
			return opMsg{text: "removed from list (transcript kept)", err: err}
		}
	case "n":
		dir := config.Expand("~")
		if s, ok := m.selected(); ok && s.Cwd != "" {
			dir = s.Cwd
		}
		m.startInput(modeNew, collapseHome(dir))
	case "e":
		if s, ok := m.selected(); ok {
			m.startInput(modeRename, s.Name)
		}
	}
	return m, nil
}

func (m *sidebar) startInput(mode inputMode, value string) {
	m.mode = mode
	m.compl.reset()
	if mode == modeNew && value != "" && !strings.HasSuffix(value, "/") {
		value += "/" // ready to Tab into the directory's children
	}
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Width = max(m.width-4, 10)
	m.input.Focus()
}

func (m *sidebar) inputKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeNew {
		delta := map[string]int{"tab": 1, "down": 1, "shift+tab": -1, "up": -1}[k.String()]
		if delta != 0 {
			m.input.SetValue(m.compl.next(m.input.Value(), delta))
			m.input.CursorEnd()
			return m, nil
		}
		m.compl.reset()
	}
	switch k.String() {
	case "esc", "ctrl+c":
		m.mode = modeNormal
		m.input.Blur()
		return m, nil
	case "enter":
		mode, value := m.mode, strings.TrimSpace(m.input.Value())
		m.mode = modeNormal
		m.input.Blur()
		if mode == modeNew {
			return m, m.create(value)
		}
		return m, m.rename(value)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m *sidebar) create(dirArg string) tea.Cmd {
	cfg, st, self, width := m.cfg, m.st, m.self, m.cfg.SidebarWidth
	names := m.paneNames()
	return func() tea.Msg {
		dir := resolveDir(dirArg)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return opMsg{err: fmt.Errorf("not a directory: %s", dirArg)}
		}
		pane, prof, err := newSession(cfg, st, dir, "", "", nil)
		if err != nil {
			return opMsg{err: err}
		}
		if self != "" {
			err = tmux.Show(self, pane, width, func(p string) string { return names[p] }, true)
		}
		return opMsg{text: fmt.Sprintf("started in %s [%s]", filepath.Base(dir), prof.Name), err: err, selectPane: pane}
	}
}

func (m *sidebar) rename(name string) tea.Cmd {
	s, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.st
	return func() tea.Msg {
		err := st.Tx(func(tx *store.Tx) error {
			cur, ok, err := tx.Get(s.ID)
			if err != nil || !ok {
				return err
			}
			cur.Name = name
			return tx.Put(cur)
		})
		return opMsg{err: err}
	}
}

// open shows the selected session next to the sidebar, resuming it first
// when it is not running. focus moves the keyboard into the session.
func (m *sidebar) open(focus bool) tea.Cmd {
	s, ok := m.selected()
	if !ok {
		return nil
	}
	if m.self == "" {
		m.flash("not inside `harness ui` — use harness attach", true)
		return nil
	}
	cfg, st, self, width := m.cfg, m.st, m.self, m.cfg.SidebarWidth
	names := m.paneNames()
	return func() tea.Msg {
		pane, text := s.TmuxPane, ""
		if pane == "" || !paneExists(pane) {
			if s.Live() && s.PID != 0 {
				return askMoveMsg{id: s.ID}
			}
			var err error
			if pane, err = resumeSession(cfg, st, s); err != nil {
				return opMsg{err: err}
			}
			text = "resumed " + label(s)
		}
		err := tmux.Show(self, pane, width, func(p string) string { return names[p] }, focus)
		return opMsg{text: text, err: err, selectPane: pane}
	}
}

// moveHere takes a terminal-tab session over (see moveSession) and shows it.
func (m *sidebar) moveHere(id string) tea.Cmd {
	s, ok := m.byID(id)
	if !ok {
		return nil
	}
	cfg, st, self, width := m.cfg, m.st, m.self, m.cfg.SidebarWidth
	names := m.paneNames()
	m.flash("moving "+label(s)+"…", false)
	return func() tea.Msg {
		pane, err := moveSession(cfg, st, s)
		if err != nil {
			return opMsg{err: err}
		}
		err = tmux.Show(self, pane, width, func(p string) string { return names[p] }, true)
		return opMsg{text: "moved " + label(s) + " into harness", err: err, selectPane: pane}
	}
}

func (m *sidebar) byID(id string) (store.Session, bool) {
	for _, s := range m.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return store.Session{}, false
}

type peekDoneMsg struct{ err error }

// peekMovedMsg: ↑/↓ in the peek chose another session (SIGUSR2).
type peekMovedMsg struct{}

// peekFile carries the session on screen from the peek to the sidebar.
func (m *sidebar) peekFile() string { return filepath.Join(m.cfg.Home, "peek.sel") }

// followPeek selects the session the peek shows.
func (m *sidebar) followPeek() {
	if id, err := os.ReadFile(m.peekFile()); err == nil {
		for _, s := range m.visible() {
			if s.ID == string(id) {
				m.selID, m.lastPane = s.ID, s.TmuxPane
			}
		}
	}
}

// peek opens the hold-space preview of s over the right-hand slot; ↑/↓
// inside it walk the current tab's sessions in sidebar order.
func (m *sidebar) peek(s store.Session) tea.Cmd {
	if m.peeking || m.self == "" {
		return nil
	}
	m.peeking = true
	var ids []string
	for _, v := range m.visible() {
		ids = append(ids, v.ID)
	}
	os.Remove(m.peekFile())
	self, width, cmd := m.self, m.cfg.SidebarWidth, previewCommand(s.ID, ids, m.peekFile())
	return func() tea.Msg {
		return peekDoneMsg{tmux.Popup(self, width, "␣ peek", cmd)}
	}
}

// jump shows the n-th active session (the number in the sidebar, ⌥n).
func (m *sidebar) jump(n int) tea.Cmd {
	order := switchOrder(m.sessions, time.Now())
	if n > len(order) || m.self == "" {
		return nil
	}
	target, self, width := order[n-1], m.self, m.cfg.SidebarWidth
	names := m.paneNames()
	m.selPane = target.TmuxPane
	return func() tea.Msg {
		err := tmux.Show(self, target.TmuxPane, width, func(p string) string { return names[p] }, true)
		return opMsg{err: err, selectPane: target.TmuxPane}
	}
}

// numbers maps session ids to their 1…9 shortcut.
func (m *sidebar) numbers() map[string]int {
	nums := map[string]int{}
	for i, s := range switchOrder(m.sessions, time.Now()) {
		if i == 9 {
			break
		}
		nums[s.ID] = i + 1
	}
	return nums
}

func (m *sidebar) paneNames() map[string]string {
	names := map[string]string{}
	for _, s := range m.sessions {
		if s.TmuxPane != "" {
			names[s.TmuxPane] = windowName(s)
		}
	}
	return names
}

func (m *sidebar) mouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch {
	case ev.Button == tea.MouseButtonWheelUp:
		m.move(-1)
	case ev.Button == tea.MouseButtonWheelDown:
		m.move(1)
	case ev.Button == tea.MouseButtonLeft && ev.Action == tea.MouseActionPress && ev.Y == 0:
		for t, hit := range m.tabHits {
			if ev.X >= hit[0] && ev.X < hit[1] {
				m.setTab(tab(t))
			}
		}
	case ev.Button == tea.MouseButtonLeft && ev.Action == tea.MouseActionPress:
		_, owners := m.body()
		i := ev.Y - headerLines + m.offset
		if i >= 0 && i < len(owners) && owners[i] != "" {
			if owners[i] == m.selID {
				return m, m.open(false)
			}
			m.selID = owners[i]
		}
	}
	return m, nil
}

// ---- selection ----

// visible returns the current tab's sessions in display order.
func (m *sidebar) visible() []store.Session {
	return flatten(sections(m.sessions, m.tab, m.showEnded, time.Now()))
}

// selectShown moves the selection to the session shown on the right: when
// the keyboard goes there, the sidebar should mark where it went.
func (m *sidebar) selectShown() {
	if m.shown == "" {
		return
	}
	for _, s := range m.sessions {
		if s.TmuxPane == m.shown && s.Live() {
			m.setTab(tabSessions)
			m.selID, m.lastPane, m.selPane = s.ID, s.TmuxPane, ""
			return
		}
	}
}

func (m *sidebar) setTab(t tab) {
	if t == m.tab {
		return
	}
	m.selByTab[m.tab] = m.selID
	m.tab, m.selID, m.lastPane, m.offset = t, m.selByTab[t], "", 0
	m.fixSelection()
}

func (m *sidebar) selected() (store.Session, bool) {
	for _, s := range m.visible() {
		if s.ID == m.selID {
			return s, true
		}
	}
	return store.Session{}, false
}

// fixSelection keeps the selection on the same session across refreshes.
// Session ids change under us (placeholder → real id on SessionStart, or a
// resume that forks), so a vanished id is followed through its tmux pane.
func (m *sidebar) fixSelection() {
	vis := m.visible()
	find := func(match func(store.Session) bool) bool {
		for _, s := range vis {
			if match(s) {
				m.selID, m.lastPane = s.ID, s.TmuxPane
				return true
			}
		}
		return false
	}
	if m.selPane != "" {
		if !find(func(s store.Session) bool { return s.TmuxPane == m.selPane }) {
			return // not in the data yet; keep waiting
		}
		m.selPane = ""
		return
	}
	if find(func(s store.Session) bool { return s.ID == m.selID }) {
		return
	}
	if m.lastPane != "" && find(func(s store.Session) bool { return s.TmuxPane == m.lastPane }) {
		return
	}
	if len(vis) > 0 {
		m.selID, m.lastPane = vis[0].ID, vis[0].TmuxPane
	}
}

func (m *sidebar) move(delta int) {
	vis := m.visible()
	for i, s := range vis {
		if s.ID == m.selID {
			m.moveTo(i + delta)
			return
		}
	}
	m.moveTo(0)
}

func (m *sidebar) moveTo(i int) {
	vis := m.visible()
	if len(vis) == 0 {
		return
	}
	i = min(max(i, 0), len(vis)-1)
	m.selID = vis[i].ID
}

// ---- view ----

const headerLines = 2

// Truecolor palette; tmux downsamples when the outer terminal cannot show it.
var (
	cAccent = lipgloss.Color("#7AA2FF")
	cText   = lipgloss.Color("#E6E9F2")
	cSub    = lipgloss.Color("#A3ADC8") // secondary text: snippets, ages
	cMuted  = lipgloss.Color("#7C849C") // stopped sessions
	cLine   = lipgloss.Color("#3A4160") // separators
	cSelBg  = lipgloss.Color("#26304F")

	stTitle = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stText  = lipgloss.NewStyle().Foreground(cText)
	stSub   = lipgloss.NewStyle().Foreground(cSub)
	stMuted = lipgloss.NewStyle().Foreground(cMuted)
	stLine  = lipgloss.NewStyle().Foreground(cLine)
	stKey   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	stSectn = lipgloss.NewStyle().Foreground(cSub).Bold(true)
	stErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B81")).Bold(true)
	stOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("#5EE38F"))

	stStatus = map[string]lipgloss.Style{
		store.Working:  lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB547")),
		store.Waiting:  lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5C8A")).Bold(true),
		store.Idle:     lipgloss.NewStyle().Foreground(lipgloss.Color("#5EE38F")),
		store.Starting: lipgloss.NewStyle().Foreground(lipgloss.Color("#4CC9F0")),
		store.Archived: lipgloss.NewStyle().Foreground(cMuted),
		store.Ended:    lipgloss.NewStyle().Foreground(lipgloss.Color("#5A6280")),
	}
	glyph = map[string]string{
		store.Working: "●", store.Waiting: "◆", store.Idle: "○",
		store.Starting: "◌", store.Archived: "▪", store.Ended: "·",
	}

	// Each profile gets a stable colour for its header.
	profileColors = []lipgloss.Color{"#7AA2FF", "#FFB547", "#C792EA", "#4FD6BE", "#FF7EB6", "#9ECE6A"}
)

func profileStyle(name string) lipgloss.Style {
	if name == "other" {
		return lipgloss.NewStyle().Bold(true).Foreground(cSub)
	}
	h := 0
	for _, r := range name {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return lipgloss.NewStyle().Bold(true).Foreground(profileColors[h%len(profileColors)])
}

func (m *sidebar) View() string {
	w := max(m.width, 20)
	lines, owners := m.body()
	if m.mode == modeNew {
		lines, owners = m.completionBody(w)
	}

	bodyH := max(m.height-headerLines-m.footerLines(), 1)
	// keep the selected session (both its lines) in view
	first, last := -1, -1
	for i, id := range owners {
		if id == m.selID && id != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		if first < m.offset {
			m.offset = first
		}
		if last >= m.offset+bodyH {
			m.offset = last - bodyH + 1
		}
	}
	m.offset = min(max(m.offset, 0), max(len(lines)-bodyH, 0))

	var b strings.Builder
	b.WriteString(m.header(w) + "\n")
	b.WriteString(stLine.Render(strings.Repeat("─", w)) + "\n")
	for i := m.offset; i < m.offset+bodyH; i++ {
		if i < len(lines) {
			b.WriteString(lines[i])
		}
		b.WriteString("\n")
	}
	b.WriteString(m.footer(w))
	return b.String()
}

// header shows the tabs; the right side keeps sessions waiting for the user
// visible whichever tab is open.
func (m *sidebar) header(w int) string {
	counts := [len(tabNames)]int{}
	waiting := 0
	for t := range tabNames {
		counts[t] = len(flatten(sections(m.sessions, tab(t), m.showEnded, time.Now())))
	}
	for _, s := range m.sessions {
		if s.Status == store.Waiting {
			waiting++
		}
	}
	var b strings.Builder
	col := 0
	for t, name := range tabNames {
		label := fmt.Sprintf(" %s %d ", name, counts[t])
		st := stSub
		if tab(t) == m.tab {
			st = lipgloss.NewStyle().Bold(true).Foreground(cText).Background(cSelBg)
		}
		b.WriteString(" ")
		col++
		m.tabHits[t] = [2]int{col, col + lipgloss.Width(label)}
		b.WriteString(st.Render(label))
		col += lipgloss.Width(label)
	}
	left := b.String()
	right := ""
	if waiting > 0 {
		right = stStatus[store.Waiting].Render(fmt.Sprintf("%s%d", glyph[store.Waiting], waiting)) + " "
	}
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

// completionBody lists directory candidates for the new-session prompt,
// scrolled so the highlighted one stays in view.
func (m *sidebar) completionBody(w int) ([]string, []string) {
	cands, cur := m.compl.candidates(m.input.Value())
	lines := []string{stSectn.Render(" DIRECTORIES ") + stSub.Render("⇥ complete · ↑↓ choose")}
	room := max(m.height-headerLines-m.footerLines()-1, 1)
	first := 0
	if cur >= room {
		first = cur - room + 1
	}
	for i := first; i < len(cands) && i < first+room; i++ {
		name := strings.TrimSuffix(cands[i], "/")
		name = filepath.Base(name) + "/"
		st := stText
		if i == cur {
			st = lipgloss.NewStyle().Bold(true).Foreground(cText).Background(cSelBg)
		}
		lines = append(lines, fill(st.Render("  "+ansi.Truncate(name, w-3, "…")), w, i == cur))
	}
	if len(cands) == 0 {
		lines = append(lines, stSub.Render("  no subdirectories"))
	}
	return lines, make([]string, len(lines))
}

// body renders all session lines; owners[i] is the session id of line i.
func (m *sidebar) body() ([]string, []string) {
	w := max(m.width, 20)
	var lines, owners []string
	add := func(line, owner string) { lines, owners = append(lines, line), append(owners, owner) }
	for _, sec := range sections(m.sessions, m.tab, m.showEnded, time.Now()) {
		if len(sec.sessions) == 0 {
			continue
		}
		if len(lines) > 0 {
			add("", "")
		}
		title := fmt.Sprintf(" %s ", strings.ToUpper(sec.title))
		count := fmt.Sprintf(" %d", len(sec.sessions))
		rule := max(w-lipgloss.Width(title)-lipgloss.Width(count)-1, 0)
		add(stSectn.Render(title)+stLine.Render(strings.Repeat("─", rule))+stSub.Render(count), "")
		group := ""
		for _, s := range sec.sessions {
			if s.Profile != group {
				group = s.Profile
				add(profileStyle(group).Render("  "+group), "")
			}
			l1, l2 := m.renderSession(s, w)
			add(l1, s.ID)
			add(l2, s.ID)
		}
	}
	if len(lines) == 0 {
		if m.tab == tabArchived {
			add(stSub.Render(" nothing archived"), "")
		} else {
			add(stSub.Render(" no sessions — press ")+stKey.Render("n"), "")
		}
	}
	return lines, owners
}

func (m *sidebar) renderSession(s store.Session, w int) (string, string) {
	sel := s.ID == m.selID
	// Every segment carries the selection background itself: wrapping
	// already-styled text would lose it at each inner reset.
	on := func(st lipgloss.Style) lipgloss.Style {
		if sel {
			return st.Background(cSelBg)
		}
		return st
	}
	status := stStatus[s.Status]

	bar := on(lipgloss.NewStyle()).Render(" ")
	if s.TmuxPane != "" && s.TmuxPane == m.shown {
		bar = on(lipgloss.NewStyle().Foreground(cAccent)).Render("▌")
	}

	right := on(stSub).Render(ago(s.StatusSince))
	if s.Status == store.Working && s.Detail != "" {
		right = on(status).Render(s.Detail) + on(stSub).Render(" "+ago(s.StatusSince))
	}
	nameStyle := stText
	switch {
	case s.Status == store.Waiting:
		nameStyle = status
	case !s.Live():
		nameStyle = stMuted
	}
	if sel {
		nameStyle = nameStyle.Bold(true)
	}
	num := on(lipgloss.NewStyle()).Render("  ")
	if n := m.numbers()[s.ID]; n > 0 {
		st := stMuted
		if s.TmuxPane != "" && s.TmuxPane == m.shown {
			st = stKey
		}
		num = on(st).Render(fmt.Sprintf("%d ", n))
	}
	nameW := max(w-6-lipgloss.Width(right)-1, 4)
	name := padRight(ansi.Truncate(label(s), nameW, "…"), nameW)
	sp := on(lipgloss.NewStyle()).Render(" ")
	l1 := bar + num + on(status).Render(glyph[s.Status]) + sp + on(nameStyle).Render(name) + sp + right

	// The directory is only worth repeating when the name is not the directory.
	sub := ""
	if dir := filepath.Base(s.Cwd); dir != label(s) {
		sub = on(stMuted).Render(dir) + on(stLine).Render(" · ")
	}
	snip := snippet(s)
	if snip == "" { // nothing said yet: describe the state instead of a blank line
		snip = map[string]string{store.Starting: "starting…", store.Idle: "ready — no messages yet"}[s.Status]
	}
	if snip != "" {
		room := max(w-6-lipgloss.Width(sub), 0)
		sub += on(stSub).Render(ansi.Truncate(snip, room, "…"))
	}
	l2 := bar + on(lipgloss.NewStyle()).Render("    ") + sub

	return fill(l1, w, sel), fill(l2, w, sel)
}

// fill pads a line to the full width, in the selection colour when selected.
func fill(line string, w int, sel bool) string {
	pad := max(w-lipgloss.Width(line), 0)
	st := lipgloss.NewStyle()
	if sel {
		st = st.Background(cSelBg)
	}
	return line + st.Render(strings.Repeat(" ", pad))
}

// label is the session's primary name in the sidebar.
func label(s store.Session) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Title != "":
		return s.Title
	case s.Cwd != "":
		return filepath.Base(s.Cwd)
	}
	return short(s.ID)
}

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func (m *sidebar) footerLines() int { return 3 }

func (m *sidebar) footer(w int) string {
	var status string
	switch {
	case m.confirmMove != "":
		name := m.confirmMove
		if s, ok := m.byID(name); ok {
			name = label(s)
		}
		status = stErr.Render(ansi.Truncate(" move "+name+" here? [Y/n]", w, "…"))
	case m.mode == modeNew:
		status = stTitle.Render(" dir ") + m.input.View()
	case m.mode == modeRename:
		status = stTitle.Render(" name ") + m.input.View()
	case m.msg != "" && time.Since(m.msgAt) < 6*time.Second:
		st := stOK
		if m.msgErr {
			st = stErr
		}
		status = st.Render(" " + ansi.Truncate(m.msg, w-2, "…"))
	}
	help := keys("⏎", "open", "␣", "peek", "n", "new", "a", "archive")
	help2 := keys("1-9", "jump", "←→", "tabs", "e", "name", "q", "detach")
	if m.tab == tabArchived {
		help = keys("⏎", "resume", "␣", "hold: peek", "x", "forget", ".", "ended")
	}
	if m.mode != modeNormal {
		help, help2 = keys("⏎", "confirm", "esc", "cancel"), ""
	}
	if m.confirmMove != "" {
		help = stSub.Render(" it closes in its terminal tab")
	}
	if status == "" {
		status = help2
	}
	return stLine.Render(strings.Repeat("─", w)) + "\n" +
		ansi.Truncate(help, w, "") + "\n" + ansi.Truncate(status, w, "")
}

// keys renders "key label" pairs with the keys highlighted.
func keys(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(" " + stKey.Render(pairs[i]) + " " + stSub.Render(pairs[i+1]) + " ")
	}
	return b.String()
}

func collapseHome(p string) string {
	home := config.Expand("~")
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}
