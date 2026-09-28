package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// cmdChanges runs the git changes pane (right of the session shown next to
// the sidebar), or opens / closes it with `toggle` (⌘⇧G, `c` in the sidebar).
func cmdChanges(args []string) error {
	if len(args) == 1 && args[0] == "toggle" {
		err := withStore(func(cfg *config.Config, st *store.Store) error {
			_, err := toggleChanges(cfg)
			return err
		})
		if err != nil && tmux.Sidebar() != "" { // from a key binding: say it on the status line
			tmux.Message("harness: " + err.Error())
			return nil
		}
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: harness changes [toggle]")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		self := ""
		if tmux.Inside() {
			self = os.Getenv("TMUX_PANE")
			tmux.MarkBuild(self, buildID())
			lipgloss.SetColorProfile(termenv.TrueColor)
		}
		m := &changesPanel{cfg: cfg, st: st, self: self}
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus())
		// SIGUSR2: the diff viewer or the peek moved to another file.
		moved := make(chan os.Signal, 4)
		signal.Notify(moved, syscall.SIGUSR2)
		defer signal.Stop(moved)
		go func() {
			for range moved {
				p.Send(fileMovedMsg{})
			}
		}()
		_, err := p.Run()
		return err
	})
}

// changesLoop is the command the changes pane runs: restarted if it crashes.
func changesLoop(exe string) string {
	return fmt.Sprintf("while :; do %s changes && break; sleep 1; done", tmux.Quote(exe))
}

func toggleChanges(cfg *config.Config) (bool, error) {
	sb := tmux.Sidebar()
	if sb == "" {
		return false, errors.New("sidebar is not running")
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return false, err
	}
	return tmux.ToggleChanges(sb, cfg.SidebarWidth, cfg.ChangesWidth, changesLoop(exe))
}

// ---- git ----

type fileChange struct {
	code      string // two-letter porcelain status: "M ", " M", "??", "R ", …
	path      string // relative to the repository root
	add, del  int
	binary    bool
	untracked bool
	mtime     time.Time // zero for a deleted file
}

type gitState struct {
	root      string
	branch    string // "main", with ahead/behind: "main ↑1 ↓2"
	base      string // revision the working tree is compared to
	baseLabel string // what base is called: "HEAD" or the main branch
	commits   int    // commits on the branch since base (branch scope)
	last      string // subject of the last commit
	lastAt    time.Time
	files     []fileChange
}

// gitTimeout keeps a huge or locked repository from wedging the pane.
const gitTimeout = 3 * time.Second

// git runs read-only: GIT_OPTIONAL_LOCKS=0 stops `status` from refreshing
// the index, whose lock would collide with the session's own git commands.
func git(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return out, err
	}
	return out, nil
}

var errNotRepo = errors.New("not a git repository")

// readGit collects the changes in dir's repository: uncommitted ones
// (against HEAD), or with branch set everything the branch changed since
// it left the main branch, committed or not.
func readGit(dir string, branch bool) (gitState, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitState{}, errNotRepo
	}
	g := gitState{root: strings.TrimSpace(string(out)), base: "HEAD", baseLabel: "HEAD"}
	out, err = git(g.root, "status", "--porcelain=v1", "-z", "-b", "--untracked-files=all")
	if err != nil {
		return g, err
	}
	var files []fileChange
	g.branch, files = parseStatus(out)

	if branch {
		def := defaultBranch(g.root)
		if def == "" {
			return g, errors.New("no main branch to compare with")
		}
		mb, err := git(g.root, "merge-base", "HEAD", def)
		if err != nil {
			return g, err
		}
		g.base, g.baseLabel = strings.TrimSpace(string(mb)), strings.TrimPrefix(def, "origin/")
		if n, err := git(g.root, "rev-list", "--count", g.base+"..HEAD"); err == nil {
			g.commits, _ = strconv.Atoi(strings.TrimSpace(string(n)))
		}
		out, err := git(g.root, "diff", "--name-status", "-z", "-M", g.base)
		if err != nil {
			return g, err
		}
		tracked := parseNameStatus(out)
		for _, f := range files {
			if f.untracked {
				tracked = append(tracked, f)
			}
		}
		files = tracked
	}

	// Staged and unstaged together, against base (absent in a fresh repo).
	if out, err := git(g.root, "diff", "--numstat", "-z", "-M", g.base); err == nil {
		stats := parseNumstat(out)
		for i, f := range files {
			if n, ok := stats[f.path]; ok {
				files[i].add, files[i].del, files[i].binary = n.add, n.del, n.binary
			}
		}
	}
	for i, f := range files {
		if f.untracked { // not in the diff: every line of them is new
			files[i].add, files[i].binary = countLines(filepath.Join(g.root, f.path))
		}
		if fi, err := os.Stat(filepath.Join(g.root, f.path)); err == nil {
			files[i].mtime = fi.ModTime()
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].path < files[j].path })
	g.files = files

	if out, err := git(g.root, "log", "-1", "--format=%s%x00%ct"); err == nil {
		subject, ts, _ := strings.Cut(strings.TrimSpace(string(out)), "\x00")
		g.last = subject
		if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
			g.lastAt = time.Unix(n, 0)
		}
	}
	return g, nil
}

// defaultBranch is what "the branch's changes" are measured against:
// origin's default branch, else a local main or master.
func defaultBranch(root string) string {
	if out, err := git(root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(string(out))
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	return ""
}

// untrackedMax caps how much of an untracked file is read to count its lines.
const untrackedMax = 1 << 20

func countLines(path string) (int, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > untrackedMax {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, true
	}
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n, false
}

// parseStatus reads `git status --porcelain=v1 -z -b`: a "## branch" entry,
// then "XY path" entries; a rename or copy is followed by its old path.
func parseStatus(out []byte) (string, []fileChange) {
	var branch string
	var files []fileChange
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		switch {
		case strings.HasPrefix(e, "## "):
			branch = parseBranch(e[3:])
		case len(e) > 3:
			code := e[:2]
			files = append(files, fileChange{code: code, path: e[3:], untracked: code == "??"})
			if code[0] == 'R' || code[0] == 'C' {
				i++ // the old path
			}
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].path < files[j].path })
	return branch, files
}

// parseNameStatus reads `git diff --name-status -z`: a status ("M", "A",
// "R100", …) then the path, or the old and the new path for a rename/copy.
func parseNameStatus(out []byte) []fileChange {
	var files []fileChange
	entries := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(entries); i++ {
		st := entries[i]
		if st == "" {
			continue
		}
		path := entries[i+1]
		i++
		if (st[0] == 'R' || st[0] == 'C') && i+1 < len(entries) {
			path = entries[i+1]
			i++
		}
		files = append(files, fileChange{code: st[:1] + " ", path: path})
	}
	return files
}

// parseBranch turns "main...origin/main [ahead 1, behind 2]" into "main ↑1 ↓2".
func parseBranch(s string) string {
	head, track, _ := strings.Cut(s, " [")
	name, _, _ := strings.Cut(head, "...")
	name = strings.TrimPrefix(name, "No commits yet on ")
	var b strings.Builder
	b.WriteString(name)
	for _, part := range strings.Split(strings.TrimSuffix(track, "]"), ", ") {
		if n, ok := strings.CutPrefix(part, "ahead "); ok {
			b.WriteString(" ↑" + n)
		}
		if n, ok := strings.CutPrefix(part, "behind "); ok {
			b.WriteString(" ↓" + n)
		}
	}
	return b.String()
}

type numstat struct {
	add, del int
	binary   bool
}

// parseNumstat reads `git diff --numstat -z`: "add\tdel\tpath" entries, or
// "add\tdel\t" followed by the old and the new path for a rename; binary
// files count "-".
func parseNumstat(out []byte) map[string]numstat {
	stats := map[string]numstat{}
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		f := strings.SplitN(entries[i], "\t", 3)
		if len(f) != 3 {
			continue
		}
		path := f[2]
		if path == "" && i+2 < len(entries) { // rename: keyed by the new path
			path = entries[i+2]
			i += 2
		}
		add, err1 := strconv.Atoi(f[0])
		del, err2 := strconv.Atoi(f[1])
		stats[path] = numstat{add, del, err1 != nil || err2 != nil}
	}
	return stats
}

// ---- panel ----

type changesPanel struct {
	cfg  *config.Config
	st   *store.Store
	self string

	sidebar string
	pane    string // session pane the changes are shown for
	title   string // its label
	cwd     string
	git     gitState
	err     error

	branchScope bool // everything since the main branch, not only uncommitted
	recent      bool // newest edits first instead of the directory tree
	focused     bool

	selPath  string
	clicked  string // file pressed while already selected: opens on release
	offset   int
	loading  bool
	lastLoad time.Time
	stamp    string
	popup    bool // a diff, peek or editor popup is open
	msg      string
	msgErr   bool
	msgAt    time.Time

	width, height int
}

type probeMsg struct{ sidebar, pane string }

type changesMsg struct {
	pane, title, cwd string
	branch           bool
	git              gitState
	err              error
}

type popupDoneMsg struct{ err error }

// fileMovedMsg: the viewer or the peek is now on another file (SIGUSR2).
type fileMovedMsg struct{}

const (
	probeEvery     = 250 * time.Millisecond // which session is shown, and state.db
	changesRefresh = 3 * time.Second        // edits made outside Claude
	freshFor       = 2 * time.Minute        // an edit this recent gets a dot
)

func (m *changesPanel) Init() tea.Cmd { return m.probe() }

// probe finds the session shown next to the sidebar (one cheap tmux call).
func (m *changesPanel) probe() tea.Cmd {
	sidebar := m.sidebar
	return tea.Tick(probeEvery, func(time.Time) tea.Msg {
		if sidebar == "" || !paneExists(sidebar) {
			sidebar = tmux.Sidebar()
		}
		if sidebar == "" {
			return probeMsg{}
		}
		return probeMsg{sidebar: sidebar, pane: tmux.Shown(sidebar)}
	})
}

// load reads the shown session's directory and its repository. Claude's
// PostToolUse hook writes state.db after every edit, so its stamp changing
// is the signal that files may have changed.
func (m *changesPanel) load(pane string) tea.Cmd {
	st, branch := m.st, m.branchScope
	return func() tea.Msg {
		msg := changesMsg{pane: pane, branch: branch}
		if pane == "" {
			return msg
		}
		ss, err := st.List()
		if err != nil {
			msg.err = err
			return msg
		}
		for _, s := range ss {
			if s.TmuxPane == pane && s.Live() {
				msg.title, msg.cwd = label(s), s.Cwd
			}
		}
		if msg.cwd == "" {
			msg.err = errors.New("unknown session")
			return msg
		}
		msg.git, msg.err = readGit(msg.cwd, branch)
		return msg
	}
}

func (m *changesPanel) reload() tea.Cmd {
	if m.loading {
		m.lastLoad = time.Time{} // again once the current load ends
		return nil
	}
	m.loading = true
	return m.load(m.pane)
}

func (m *changesPanel) flash(text string, isErr bool) {
	m.msg, m.msgErr, m.msgAt = text, isErr, time.Now()
}

func (m *changesPanel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case probeMsg:
		m.sidebar = msg.sidebar
		stamp := stateStamp(m.cfg.Home)
		due := msg.pane != m.pane || stamp != m.stamp || time.Since(m.lastLoad) >= changesRefresh
		if !m.loading && due {
			if msg.pane != m.pane {
				m.selPath, m.offset = "", 0
			}
			m.loading, m.stamp = true, stamp
			return m, tea.Batch(m.load(msg.pane), m.probe())
		}
		return m, m.probe()
	case changesMsg:
		m.loading, m.lastLoad = false, time.Now()
		if msg.branch != m.branchScope { // the scope changed while loading
			m.lastLoad = time.Time{}
			return m, nil
		}
		m.pane, m.title, m.cwd, m.git, m.err = msg.pane, msg.title, msg.cwd, msg.git, msg.err
		m.fixSelection()
	case popupDoneMsg:
		m.popup = false
		m.followViewer()
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
		}
	case fileMovedMsg:
		m.followViewer()
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *changesPanel) mouse(ev tea.MouseMsg) tea.Cmd {
	switch {
	case ev.Button == tea.MouseButtonWheelUp:
		m.move(-1)
	case ev.Button == tea.MouseButtonWheelDown:
		m.move(1)
	case ev.Button == tea.MouseButtonLeft && ev.Action == tea.MouseActionPress:
		m.clicked = ""
		rows := m.rows()
		if i := ev.Y - changesHeader + m.offset; i >= 0 && i < len(rows) && rows[i].file >= 0 {
			f := m.git.files[rows[i].file]
			if f.path == m.selPath {
				m.clicked = f.path
			}
			m.selPath = f.path
		}
	case ev.Action == tea.MouseActionRelease:
		// Open on release: tmux closes a popup when the button is let go
		// outside it, and this pane is outside the diff popup.
		open := m.clicked != "" && m.clicked == m.selPath
		m.clicked = ""
		if open {
			return m.viewDiff()
		}
	}
	return nil
}

func (m *changesPanel) key(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "q", "c":
		cfg := m.cfg
		return func() tea.Msg {
			_, err := toggleChanges(cfg) // kills this pane
			return popupDoneMsg{err}
		}
	case "esc", "tab": // back into the session
		if pane := m.pane; pane != "" {
			return func() tea.Msg { return popupDoneMsg{tmux.Focus(pane)} }
		}
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home", "g":
		m.moveTo(0)
	case "end", "G":
		m.moveTo(len(m.order()) - 1)
	case "enter", "d":
		return m.viewDiff()
	case " ":
		return m.peekDiff()
	case "b":
		m.branchScope = !m.branchScope
		m.selPath, m.offset = "", 0
		return m.reload()
	case "s":
		m.recent = !m.recent
	case "r":
		return m.reload()
	case "e":
		return m.edit()
	case "y":
		if f, ok := m.selected(); ok {
			if err := copyText(f.path); err != nil {
				m.flash(err.Error(), true)
			} else {
				m.flash("copied "+f.path, false)
			}
		}
	}
	return nil
}

// order lists the files in display order (what ↑/↓ and the viewer walk).
func (m *changesPanel) order() []int {
	var out []int
	for _, r := range m.rows() {
		if r.file >= 0 {
			out = append(out, r.file)
		}
	}
	return out
}

func (m *changesPanel) selected() (fileChange, bool) {
	for _, f := range m.git.files {
		if f.path == m.selPath {
			return f, true
		}
	}
	return fileChange{}, false
}

func (m *changesPanel) index() int {
	for i, fi := range m.order() {
		if m.git.files[fi].path == m.selPath {
			return i
		}
	}
	return -1
}

func (m *changesPanel) fixSelection() {
	if _, ok := m.selected(); !ok {
		m.selPath = ""
		m.moveTo(0)
	}
}

func (m *changesPanel) move(delta int) { m.moveTo(max(m.index(), 0) + delta) }

func (m *changesPanel) moveTo(i int) {
	order := m.order()
	if len(order) == 0 {
		return
	}
	m.selPath = m.git.files[order[min(max(i, 0), len(order)-1)]].path
}

// resultFile is where the viewer and the peek leave the file they are on.
func (m *changesPanel) resultFile() string {
	return filepath.Join(m.cfg.Home, "changes"+strings.TrimPrefix(m.self, "%")+".sel")
}

func (m *changesPanel) followViewer() {
	if path, err := os.ReadFile(m.resultFile()); err == nil {
		for _, f := range m.git.files {
			if f.path == string(path) {
				m.selPath = f.path
			}
		}
	}
}

// writeList hands the files, in display order, to `harness diff`.
func (m *changesPanel) writeList() (string, error) {
	order := m.order()
	if len(order) == 0 || m.index() < 0 {
		return "", errors.New("no file selected")
	}
	list := diffList{Root: m.git.root, Base: m.git.base, At: m.index(), Result: m.resultFile(), PID: os.Getpid()}
	for _, fi := range order {
		f := m.git.files[fi]
		list.Files = append(list.Files, diffFile{Path: f.path, Code: f.code, Untracked: f.untracked, Add: f.add, Del: f.del})
	}
	data, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.Home, "changes"+strings.TrimPrefix(m.self, "%")+".json")
	os.Remove(list.Result)
	return path, os.WriteFile(path, data, 0o644)
}

// popupRun opens command in a popup over the session; the pane waits.
func (m *changesPanel) popupRun(title, command string) tea.Cmd {
	if m.popup || m.sidebar == "" {
		return nil
	}
	m.popup = true
	sidebar, width := m.sidebar, m.cfg.SidebarWidth
	return func() tea.Msg { return popupDoneMsg{tmux.Popup(sidebar, width, title, command)} }
}

func (m *changesPanel) harnessCmd(args ...string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "harness"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	parts := []string{tmux.Quote(exe)}
	for _, a := range args {
		parts = append(parts, tmux.Quote(a))
	}
	return strings.Join(parts, " ")
}

// viewDiff opens the scrollable diff viewer on the selected file.
func (m *changesPanel) viewDiff() tea.Cmd {
	list, err := m.writeList()
	if err != nil {
		return nil
	}
	return m.popupRun("diff", m.harnessCmd("diff", list))
}

// peekDiff shows the selected file's diff while space is held.
func (m *changesPanel) peekDiff() tea.Cmd {
	list, err := m.writeList()
	if err != nil {
		return nil
	}
	return m.popupRun("␣ peek", m.harnessCmd("diff", "--hold", list))
}

// edit opens the selected file in $EDITOR, in a popup over the session.
func (m *changesPanel) edit() tea.Cmd {
	f, ok := m.selected()
	if !ok || f.mtime.IsZero() {
		return nil
	}
	cmd := "cd " + tmux.Quote(m.git.root) + " && exec ${VISUAL:-${EDITOR:-vi}} " + tmux.Quote(f.path)
	return m.popupRun(f.path, cmd)
}

func copyText(s string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// ---- view ----

const changesHeader = 5

var stCode = map[byte]lipgloss.Style{
	'M': lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB547")),
	'A': lipgloss.NewStyle().Foreground(lipgloss.Color("#5EE38F")),
	'?': lipgloss.NewStyle().Foreground(lipgloss.Color("#5EE38F")),
	'D': lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B81")),
	'R': lipgloss.NewStyle().Foreground(lipgloss.Color("#4CC9F0")),
	'C': lipgloss.NewStyle().Foreground(lipgloss.Color("#4CC9F0")),
	'U': lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5C8A")).Bold(true),
	'T': lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB547")),
}

// codeLetter is the one letter shown for a porcelain status: the worktree
// side when it has changed, else the index side; conflicts are U.
func codeLetter(code string) byte {
	switch {
	case code == "??":
		return '?'
	case code[0] == 'U' || code[1] == 'U' || code == "AA" || code == "DD":
		return 'U'
	case code[1] != ' ':
		return code[1]
	case code[0] != ' ':
		return code[0]
	}
	return ' '
}

// row is a line of the list: a directory heading (file -1) or a file.
type row struct {
	dir  string
	file int
}

// rows lays the files out: grouped under their directory (files at the
// root first), or with s newest edit first.
func (m *changesPanel) rows() []row {
	files := m.git.files
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	if m.recent {
		sort.SliceStable(idx, func(a, b int) bool { return files[idx[a]].mtime.After(files[idx[b]].mtime) })
		out := make([]row, len(idx))
		for i, fi := range idx {
			out[i] = row{file: fi}
		}
		return out
	}
	dirOf := func(i int) string {
		if d := filepath.Dir(files[i].path); d != "." {
			return d
		}
		return ""
	}
	sort.SliceStable(idx, func(a, b int) bool {
		da, db := dirOf(idx[a]), dirOf(idx[b])
		if da != db {
			return da == "" || (db != "" && da < db)
		}
		return files[idx[a]].path < files[idx[b]].path
	})
	var out []row
	cur := ""
	for _, fi := range idx {
		if d := dirOf(fi); d != cur {
			cur = d
			out = append(out, row{dir: d, file: -1})
		}
		out = append(out, row{file: fi})
	}
	return out
}

func (m *changesPanel) View() string {
	w := max(m.width, 20)
	var b strings.Builder
	for _, l := range m.header(w) {
		b.WriteString(l + "\n")
	}

	body := m.body(w)
	bodyH := max(m.height-changesHeader-3, 1)
	sel := -1
	for i, r := range m.rows() {
		if r.file >= 0 && m.git.files[r.file].path == m.selPath {
			sel = i
		}
	}
	if sel >= 0 {
		if sel-1 < m.offset { // keep the directory heading above in view
			m.offset = max(sel-1, 0)
		}
		if sel >= m.offset+bodyH {
			m.offset = sel - bodyH + 1
		}
	}
	m.offset = min(max(m.offset, 0), max(len(body)-bodyH, 0))
	for i := m.offset; i < m.offset+bodyH; i++ {
		if i < len(body) {
			b.WriteString(body[i])
		}
		b.WriteString("\n")
	}

	b.WriteString(stLine.Render(strings.Repeat("─", w)) + "\n")
	other := map[bool]string{false: "branch", true: "uncommitted"}[m.branchScope]
	sorting := map[bool]string{false: "recent", true: "tree"}[m.recent]
	help := keys("⏎", "diff", "␣", "peek", "b", other, "s", sorting)
	help2 := keys("e", "edit", "y", "copy", "esc", "back", "c", "close")
	if m.msg != "" && time.Since(m.msgAt) < 5*time.Second {
		st := stOK
		if m.msgErr {
			st = stErr
		}
		help2 = st.Render(" " + ansi.Truncate(m.msg, w-2, "…"))
	}
	b.WriteString(ansi.Truncate(help, w, "") + "\n" + ansi.Truncate(help2, w, ""))
	return b.String()
}

// header: title with the scope, the session, its branch, the totals.
func (m *changesPanel) header(w int) []string {
	titleSt := stSectn
	if m.focused {
		titleSt = stTitle
	}
	title := titleSt.Render(" CHANGES ")
	scope := "uncommitted"
	if m.branchScope {
		scope = "since " + m.git.baseLabel
		if m.git.baseLabel == "HEAD" || m.git.baseLabel == "" {
			scope = "branch"
		}
	}
	scopeR := stSub.Render(" " + scope + " ")
	rule := stLine.Render(strings.Repeat("─", max(w-lipgloss.Width(title)-lipgloss.Width(scopeR), 0)))

	who := stMuted.Render(" no session")
	if m.title != "" {
		who = stText.Bold(true).Render(ansi.Truncate(" "+m.title, w, "…"))
	}
	branch := ""
	if m.git.branch != "" {
		branch = " " + stAccentSub.Render("⎇ "+m.git.branch)
		if m.branchScope && m.git.commits > 0 {
			branch += stSub.Render(fmt.Sprintf(" · %d commit%s", m.git.commits, map[bool]string{true: "", false: "s"}[m.git.commits == 1]))
		}
	}
	totals := ""
	if n := len(m.git.files); n > 0 {
		add, del := 0, 0
		for _, f := range m.git.files {
			add, del = add+f.add, del+f.del
		}
		totals = " " + stText.Render(fmt.Sprintf("%d file%s", n, map[bool]string{true: "", false: "s"}[n == 1])) +
			"  " + statCounts(add, del, false) + "  " + statBar(add, del)
	}
	return []string{
		title + rule + scopeR,
		who,
		ansi.Truncate(branch, w, "…"),
		ansi.Truncate(totals, w, "…"),
		"",
	}
}

func (m *changesPanel) body(w int) []string {
	switch {
	case m.pane == "":
		return []string{stSub.Render(" show a session to see its changes")}
	case errors.Is(m.err, errNotRepo):
		return []string{stSub.Render(ansi.Truncate(" "+collapseHome(m.cwd)+" is not a git repository", w, "…"))}
	case m.err != nil:
		return []string{stErr.Render(ansi.Truncate(" "+m.err.Error(), w, "…"))}
	case len(m.git.files) == 0 && m.git.root != "":
		clean := " ✓ working tree clean"
		if m.branchScope {
			clean = " ✓ nothing changed since " + m.git.baseLabel
		}
		out := []string{stOK.Render(clean)}
		if m.git.last != "" {
			out = append(out, "", stSub.Render(" last commit")+stMuted.Render(" · "+ago(m.git.lastAt)),
				stText.Render(ansi.Truncate(" "+m.git.last, w, "…")))
		}
		return out
	}
	var out []string
	for _, r := range m.rows() {
		if r.file < 0 {
			dir := r.dir
			if dir == "" {
				continue
			}
			out = append(out, stMuted.Render(" ▾ ")+stSub.Render(truncateLeft(dir+"/", w-4)))
			continue
		}
		out = append(out, m.renderFile(m.git.files[r.file], w))
	}
	return out
}

// renderFile: status letter, file name (a dot when just edited), then the
// line counts and a change bar, or in recent order the directory and age.
func (m *changesPanel) renderFile(f fileChange, w int) string {
	sel := f.path == m.selPath
	bgOn := sel && m.focused
	on := func(st lipgloss.Style) lipgloss.Style {
		if bgOn {
			return st.Background(cSelBg)
		}
		return st
	}
	sp := on(lipgloss.NewStyle()).Render(" ")

	// selection marker, indent, status
	mark := sp
	if sel {
		mark = on(lipgloss.NewStyle().Foreground(cAccent)).Render("▌")
	}
	indent := on(lipgloss.NewStyle()).Render("   ")
	if m.recent || filepath.Dir(f.path) == "." {
		indent = sp
	}
	letter := codeLetter(f.code)
	left := mark + indent + on(stCode[letter]).Render(string(letter)) + sp

	// right side
	var right string
	switch {
	case f.binary:
		right = on(stSub).Render("bin")
	case m.recent:
		right = on(stMuted).Render(ago(f.mtime))
		if f.mtime.IsZero() {
			right = on(stMuted).Render("gone")
		}
	default:
		right = statCounts(f.add, f.del, f.untracked)
		if bgOn { // re-render on the selection background
			right = on(stOK).Render(fmt.Sprintf("+%d", f.add))
			if f.add == 0 && !f.untracked {
				right = ""
			}
			if f.del > 0 {
				if right != "" {
					right += sp
				}
				right += on(stDel).Render(fmt.Sprintf("−%d", f.del))
			}
		}
	}
	right += sp

	// name, fresh dot, directory (recent order only)
	nameStyle := stText
	switch {
	case letter == 'D':
		nameStyle = stMuted.Strikethrough(true)
	case f.untracked:
		nameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#B8F2CD"))
	}
	if sel {
		nameStyle = nameStyle.Bold(true)
	}
	fresh := !f.mtime.IsZero() && time.Since(f.mtime) < freshFor
	room := max(w-lipgloss.Width(left)-lipgloss.Width(right)-1, 4)
	name := filepath.Base(f.path)
	if fresh {
		room -= 2
	}
	text := on(nameStyle).Render(ansi.Truncate(name, room, "…"))
	if fresh {
		text += sp + on(lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB547"))).Render("•")
	}
	if m.recent {
		if dir := filepath.Dir(f.path); dir != "." {
			if left := room - lipgloss.Width(name) - 1; left >= 4 {
				text += sp + on(stMuted).Render(truncateLeft(dir, left))
			}
		}
	}
	line := left + text
	pad := max(w-lipgloss.Width(line)-lipgloss.Width(right), 1)
	return fill(line+on(lipgloss.NewStyle()).Render(strings.Repeat(" ", pad))+right, w, bgOn)
}

// truncateLeft keeps the end of a path, which tells more than its start.
func truncateLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 2 {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

var (
	stDel       = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B81"))
	stAccentSub = lipgloss.NewStyle().Foreground(cAccent)
)
