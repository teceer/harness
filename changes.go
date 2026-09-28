package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
		p := tea.NewProgram(&changesPanel{cfg: cfg, st: st, self: self}, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
	code     string // two-letter porcelain status: "M ", " M", "??", "R ", …
	path     string // relative to the repository root
	add, del int
	binary   bool
}

type gitState struct {
	root   string
	branch string // "main", with ahead/behind: "main ↑1 ↓2"
	files  []fileChange
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
			return nil, errors.New(firstLine(msg))
		}
		return nil, err
	}
	return out, nil
}

var errNotRepo = errors.New("not a git repository")

func readGit(dir string) (gitState, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitState{}, errNotRepo
	}
	g := gitState{root: strings.TrimSpace(string(out))}
	out, err = git(g.root, "status", "--porcelain=v1", "-z", "-b", "--untracked-files=all")
	if err != nil {
		return g, err
	}
	g.branch, g.files = parseStatus(out)
	// Staged and unstaged together, against HEAD (absent in a fresh repo).
	if out, err := git(g.root, "diff", "--numstat", "-z", "HEAD"); err == nil {
		stats := parseNumstat(out)
		for i, f := range g.files {
			if n, ok := stats[f.path]; ok {
				g.files[i].add, g.files[i].del, g.files[i].binary = n.add, n.del, n.binary
			}
		}
	}
	// Untracked files are not in the diff: every line of them is new.
	for i, f := range g.files {
		if f.code == "??" {
			g.files[i].add, g.files[i].binary = countLines(filepath.Join(g.root, f.path))
		}
	}
	return g, nil
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
			files = append(files, fileChange{code: code, path: e[3:]})
			if code[0] == 'R' || code[0] == 'C' {
				i++ // the old path
			}
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].path < files[j].path })
	return branch, files
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

	sidebar  string
	pane     string // session pane the changes are shown for
	title    string // its label
	cwd      string
	git      gitState
	err      error
	selPath  string
	offset   int
	loading  bool
	lastLoad time.Time
	stamp    string
	diffing  bool
	msg      string

	width, height int
}

type probeMsg struct{ sidebar, pane string }

type changesMsg struct {
	pane, title, cwd string
	git              gitState
	err              error
}

type diffDoneMsg struct{ err error }

const (
	probeEvery     = 250 * time.Millisecond // which session is shown, and state.db
	changesRefresh = 3 * time.Second        // edits made outside Claude
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
	st := m.st
	return func() tea.Msg {
		msg := changesMsg{pane: pane}
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
		msg.git, msg.err = readGit(msg.cwd)
		return msg
	}
}

func (m *changesPanel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
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
		m.pane, m.title, m.cwd, m.git, m.err = msg.pane, msg.title, msg.cwd, msg.git, msg.err
		m.fixSelection()
	case diffDoneMsg:
		m.diffing = false
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
	case tea.MouseMsg:
		switch {
		case msg.Button == tea.MouseButtonWheelUp:
			m.move(-1)
		case msg.Button == tea.MouseButtonWheelDown:
			m.move(1)
		case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
			i := msg.Y - changesHeader + m.offset
			if i >= 0 && i < len(m.git.files) {
				if m.git.files[i].path == m.selPath {
					return m, m.diff()
				}
				m.selPath = m.git.files[i].path
			}
		}
	case tea.KeyMsg:
		m.msg = ""
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q", "c", "esc":
			cfg := m.cfg
			return m, func() tea.Msg {
				_, err := toggleChanges(cfg) // kills this pane
				return diffDoneMsg{err}
			}
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "home", "g":
			m.moveTo(0)
		case "end", "G":
			m.moveTo(len(m.git.files) - 1)
		case "enter", " ", "d":
			return m, m.diff()
		case "r":
			m.lastLoad = time.Time{}
		}
	}
	return m, nil
}

func (m *changesPanel) index() int {
	for i, f := range m.git.files {
		if f.path == m.selPath {
			return i
		}
	}
	return -1
}

func (m *changesPanel) fixSelection() {
	if m.index() < 0 {
		m.selPath = ""
		if len(m.git.files) > 0 {
			m.selPath = m.git.files[0].path
		}
	}
}

func (m *changesPanel) move(delta int) { m.moveTo(max(m.index(), 0) + delta) }

func (m *changesPanel) moveTo(i int) {
	if len(m.git.files) == 0 {
		return
	}
	m.selPath = m.git.files[min(max(i, 0), len(m.git.files)-1)].path
}

// diff shows the selected file's diff against HEAD in a popup over the
// session (delta when installed, less otherwise); q closes it.
func (m *changesPanel) diff() tea.Cmd {
	i := m.index()
	if i < 0 || m.diffing || m.sidebar == "" {
		return nil
	}
	m.diffing = true
	f, root, sidebar, width := m.git.files[i], m.git.root, m.sidebar, m.cfg.SidebarWidth
	return func() tea.Msg {
		return diffDoneMsg{tmux.Popup(sidebar, width, f.path, diffCommand(root, f))}
	}
}

func diffCommand(root string, f fileChange) string {
	path := tmux.Quote(f.path)
	cmd := "git --no-pager diff --color=always HEAD -- " + path
	if f.code == "??" {
		cmd = "git --no-pager diff --color=always --no-index -- /dev/null " + path
	}
	pager := "less -R"
	if _, err := exec.LookPath("delta"); err == nil {
		pager = "delta --paging=always"
	}
	return "cd " + tmux.Quote(root) + " && { " + cmd + "; } | " + pager
}

// ---- view ----

const changesHeader = 4

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

func (m *changesPanel) View() string {
	w := max(m.width, 20)
	var b strings.Builder

	// header: title and file count, then the session and branch
	count := ""
	if n := len(m.git.files); n > 0 {
		count = fmt.Sprintf(" %d", n)
	}
	title := " CHANGES "
	b.WriteString(stSectn.Render(title) + stLine.Render(strings.Repeat("─", max(w-lipgloss.Width(title)-lipgloss.Width(count)-1, 0))) + stSub.Render(count) + "\n")
	who := ""
	if m.title != "" {
		who = " " + m.title
	}
	b.WriteString(stText.Bold(true).Render(ansi.Truncate(who, w, "…")) + "\n")
	branch := ""
	if m.git.branch != "" {
		branch = " ⎇ " + m.git.branch
	}
	b.WriteString(stAccentSub.Render(ansi.Truncate(branch, w, "…")) + "\n")
	b.WriteString(m.totals(w) + "\n")

	bodyH := max(m.height-changesHeader-2, 1)
	lines := m.lines(w)
	if i := m.index(); i >= 0 {
		if i < m.offset {
			m.offset = i
		}
		if i >= m.offset+bodyH {
			m.offset = i - bodyH + 1
		}
	}
	m.offset = min(max(m.offset, 0), max(len(lines)-bodyH, 0))
	for i := m.offset; i < m.offset+bodyH; i++ {
		if i < len(lines) {
			b.WriteString(lines[i])
		}
		b.WriteString("\n")
	}

	b.WriteString(stLine.Render(strings.Repeat("─", w)) + "\n")
	foot := keys("⏎", "diff", "r", "refresh", "c", "close")
	if m.msg != "" {
		foot = stErr.Render(" " + ansi.Truncate(m.msg, w-2, "…"))
	}
	b.WriteString(ansi.Truncate(foot, w, ""))
	return b.String()
}

// totals is the summary line: +added −deleted across all files.
func (m *changesPanel) totals(w int) string {
	if len(m.git.files) == 0 {
		return ""
	}
	add, del := 0, 0
	for _, f := range m.git.files {
		add, del = add+f.add, del+f.del
	}
	return " " + stOK.Render(fmt.Sprintf("+%d", add)) + " " + stDel.Render(fmt.Sprintf("−%d", del))
}

func (m *changesPanel) lines(w int) []string {
	switch {
	case m.pane == "":
		return []string{stSub.Render(" no session shown")}
	case errors.Is(m.err, errNotRepo):
		return []string{stSub.Render(ansi.Truncate(" "+collapseHome(m.cwd)+" is not a git repository", w, "…"))}
	case m.err != nil:
		return []string{stErr.Render(ansi.Truncate(" "+m.err.Error(), w, "…"))}
	case len(m.git.files) == 0 && m.git.root != "":
		return []string{stSub.Render(" working tree clean")}
	}
	var out []string
	for _, f := range m.git.files {
		out = append(out, m.renderFile(f, w))
	}
	return out
}

// renderFile: status letter, file name, its directory (dimmed, cut first
// when short of room) and the line counts on the right.
func (m *changesPanel) renderFile(f fileChange, w int) string {
	sel := f.path == m.selPath
	on := func(st lipgloss.Style) lipgloss.Style {
		if sel {
			return st.Background(cSelBg)
		}
		return st
	}
	letter := codeLetter(f.code)
	code := on(stCode[letter]).Render(string(letter))

	var right string
	switch {
	case f.binary:
		right = on(stSub).Render("bin")
	case f.add > 0 || f.del > 0:
		if f.add > 0 {
			right = on(stOK).Render(fmt.Sprintf("+%d", f.add))
		}
		if f.del > 0 {
			if right != "" {
				right += on(lipgloss.NewStyle()).Render(" ")
			}
			right += on(stDel).Render(fmt.Sprintf("−%d", f.del))
		}
	}

	name, dir := filepath.Base(f.path), filepath.Dir(f.path)
	room := max(w-4-lipgloss.Width(right)-1, 4)
	nameStyle := stText
	if letter == 'D' {
		nameStyle = stMuted.Strikethrough(true)
	}
	if sel {
		nameStyle = nameStyle.Bold(true)
	}
	text := on(nameStyle).Render(ansi.Truncate(name, room, "…"))
	if left := room - lipgloss.Width(name) - 1; dir != "." && left >= 4 {
		text += on(lipgloss.NewStyle()).Render(" ") + on(stMuted).Render(truncateLeft(dir, left))
	}
	sp := on(lipgloss.NewStyle()).Render(" ")
	line := sp + code + sp + sp + text
	pad := max(w-lipgloss.Width(line)-lipgloss.Width(right)-1, 1)
	return fill(line+on(lipgloss.NewStyle()).Render(strings.Repeat(" ", pad))+right, w, sel)
}

// truncateLeft keeps the end of a path, which tells more than its start.
func truncateLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

var (
	stDel       = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B81"))
	stAccentSub = lipgloss.NewStyle().Foreground(cAccent)
)
