package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// ---- the file list the changes pane hands to `harness diff` ----

// diffList is written by the changes pane for the diff viewer and the
// hold-space peek: which repository, against what, and the files in the
// pane's order. The viewer writes the path it ends on to Result and sends
// SIGUSR2 to PID, so the pane's selection follows.
type diffList struct {
	Root   string     `json:"root"`
	Base   string     `json:"base"` // revision the working tree is compared to
	Files  []diffFile `json:"files"`
	At     int        `json:"at"`
	Result string     `json:"result"`
	PID    int        `json:"pid"`
}

type diffFile struct {
	Path      string `json:"path"`
	Code      string `json:"code"`
	Untracked bool   `json:"untracked"`
	Add       int    `json:"add"`
	Del       int    `json:"del"`
}

func (l *diffList) tell(path string) {
	if l.Result == "" {
		return
	}
	os.WriteFile(l.Result, []byte(path), 0o644)
	if l.PID > 0 {
		syscall.Kill(l.PID, syscall.SIGUSR2)
	}
}

// cmdDiff shows one file of a changes list: an interactive viewer, or with
// --hold the peek that stays up while space is held (↑/↓ other files).
func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	hold := fs.Bool("hold", false, "stay open while space is held")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: harness diff [--hold] <list.json>")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var list diffList
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	if len(list.Files) == 0 {
		return errors.New("no files")
	}
	list.At = min(max(list.At, 0), len(list.Files)-1)
	lipgloss.SetColorProfile(termenv.TrueColor) // inside the harness tmux
	if *hold {
		return peekDiff(&list)
	}
	v := &diffViewer{list: &list, wrap: true, context: 3}
	v.load()
	_, err = tea.NewProgram(v, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// peekDiff draws the top of the file's diff and follows ↑/↓ until space
// is let go (see holdWhileSpace).
func peekDiff(list *diffList) error {
	draw := func() {
		w, h := termSize()
		f := list.Files[list.At]
		d, err := loadDiff(list.Root, list.Base, f, 3)
		head := diffHeader(f, list.At, len(list.Files), w)
		body := []string{}
		if err != nil {
			body = append(body, stErr.Render(" "+err.Error()))
		} else {
			body = newDiffRenderer(f.Path).render(d, w, false)
		}
		room := max(h-len(head)-1, 0)
		if len(body) > room {
			body = body[:room]
		}
		lines := append(head, body...)
		for len(lines) < h-1 {
			lines = append(lines, "")
		}
		lines = append(lines, stMuted.Render(" ↑↓ other files · release space to close"))
		fmt.Print("\x1b[H\x1b[2J" + strings.Join(lines, "\r\n"))
	}
	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h")
	draw()
	return holdWhileSpace(func(delta int) {
		next := min(max(list.At+delta, 0), len(list.Files)-1)
		if next == list.At {
			return
		}
		list.At = next
		draw()
		list.tell(list.Files[next].Path)
	})
}

// ---- parsing ----

type diffLine struct {
	kind     byte // ' ' context, '+', '-', '@' hunk header, '\\' no newline
	old, new int  // line numbers, 0 when the side has none
	text     string
	word     [2]int // rune range that changed within the line, {0,0} if none
}

type fileDiff struct {
	lines  []diffLine
	binary bool
}

// diffMaxLines keeps a generated or vendored monster from stalling the view.
const diffMaxLines = 20000

// loadDiff runs git for one file: tracked files against base (the working
// tree included), untracked ones against /dev/null.
func loadDiff(root, base string, f diffFile, context int) (fileDiff, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff", "-U" + strconv.Itoa(context)}
	if f.Untracked {
		args = append(args, "--no-index", "--", "/dev/null", f.Path)
	} else {
		args = append(args, base, "--", f.Path)
	}
	out, err := git(root, args...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 { // --no-index: "there are differences"
		err = nil
	}
	if err != nil {
		return fileDiff{}, err
	}
	return parseDiff(string(out)), nil
}

// parseDiff reads a single-file unified diff.
func parseDiff(out string) fileDiff {
	var d fileDiff
	old, new := 0, 0
	inHunk := false
	for _, raw := range strings.Split(out, "\n") {
		if len(d.lines) >= diffMaxLines {
			d.lines = append(d.lines, diffLine{kind: '\\', text: "… diff cut here"})
			break
		}
		switch {
		case strings.HasPrefix(raw, "@@"):
			inHunk = true
			o, n, ctx := parseHunk(raw)
			old, new = o, n
			d.lines = append(d.lines, diffLine{kind: '@', old: o, new: n, text: ctx})
		case !inHunk:
			if strings.HasPrefix(raw, "Binary files") || strings.HasPrefix(raw, "GIT binary patch") {
				d.binary = true
			}
		case raw == "":
			// the trailing newline of git's output
		case raw[0] == '+':
			d.lines = append(d.lines, diffLine{kind: '+', new: new, text: expandTabs(raw[1:])})
			new++
		case raw[0] == '-':
			d.lines = append(d.lines, diffLine{kind: '-', old: old, text: expandTabs(raw[1:])})
			old++
		case raw[0] == ' ':
			d.lines = append(d.lines, diffLine{kind: ' ', old: old, new: new, text: expandTabs(raw[1:])})
			old, new = old+1, new+1
		case raw[0] == '\\':
			d.lines = append(d.lines, diffLine{kind: '\\', text: strings.TrimPrefix(raw, "\\ ")})
		}
	}
	markWords(d.lines)
	return d
}

// parseHunk reads "@@ -12,7 +12,9 @@ func name" into the first line numbers
// and the function context.
func parseHunk(s string) (old, new int, ctx string) {
	rest := strings.TrimPrefix(s, "@@ ")
	ranges, ctx, _ := strings.Cut(rest, " @@")
	for _, r := range strings.Fields(ranges) {
		start, _, _ := strings.Cut(r[1:], ",")
		n, _ := strconv.Atoi(start)
		if r[0] == '-' {
			old = n
		} else {
			new = n
		}
	}
	return old, new, strings.TrimSpace(ctx)
}

func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 4 - col%4
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

// markWords pairs each run of removed lines with the added run after it,
// line by line, and marks the part of each pair that differs, so an edit
// inside a line stands out from the rest of it.
func markWords(lines []diffLine) {
	for i := 0; i < len(lines); {
		if lines[i].kind != '-' {
			i++
			continue
		}
		dels := i
		for i < len(lines) && lines[i].kind == '-' {
			i++
		}
		adds := i
		for i < len(lines) && lines[i].kind == '+' {
			i++
		}
		n := min(adds-dels, i-adds)
		for k := 0; k < n; k++ {
			a, b := &lines[dels+k], &lines[adds+k]
			a.word, b.word = wordRanges(a.text, b.text)
		}
	}
}

// wordRanges finds the differing middle of two lines (after the common
// prefix and suffix), widened to whole words. Lines that differ almost
// everywhere get no mark: the line colour already says it all.
func wordRanges(a, b string) ([2]int, [2]int) {
	ra, rb := []rune(a), []rune(b)
	p := 0
	for p < len(ra) && p < len(rb) && ra[p] == rb[p] {
		p++
	}
	s := 0
	for s < len(ra)-p && s < len(rb)-p && ra[len(ra)-1-s] == rb[len(rb)-1-s] {
		s++
	}
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	for p > 0 && isWord(ra[p-1]) {
		p--
	}
	for s > 0 && isWord(ra[len(ra)-s]) && isWord(rb[len(rb)-s]) {
		s--
	}
	wa, wb := [2]int{p, len(ra) - s}, [2]int{p, len(rb) - s}
	changed := max(wa[1]-wa[0], wb[1]-wb[0])
	if changed == 0 || float64(changed) > 0.7*float64(max(len(ra), len(rb))) {
		return [2]int{}, [2]int{}
	}
	return wa, wb
}

// ---- rendering ----

var (
	cAddBg     = lipgloss.Color("#15291E")
	cAddWordBg = lipgloss.Color("#245A36")
	cAddGutter = lipgloss.Color("#1B3526")
	cDelBg     = lipgloss.Color("#2E161C")
	cDelWordBg = lipgloss.Color("#6B2533")
	cDelGutter = lipgloss.Color("#3A1C24")
	cHunkBg    = lipgloss.Color("#1A2035")
	cAdd       = lipgloss.Color("#5EE38F")
	cDel       = lipgloss.Color("#FF6B81")
)

// syntaxStyle colours code like the rest of the harness palette.
var syntaxStyle = func() *chroma.Style {
	if s := styles.Get("tokyonight-night"); s != nil {
		return s
	}
	return styles.Fallback
}()

type diffRenderer struct {
	lexer chroma.Lexer
}

func newDiffRenderer(path string) *diffRenderer {
	l := lexers.Match(path)
	if l != nil {
		l = chroma.Coalesce(l)
	}
	return &diffRenderer{lexer: l}
}

// piece is a run of text in one style.
type piece struct {
	text  string
	style lipgloss.Style
}

// tokens splits a line into syntax-coloured pieces. Lines are lexed one by
// one, so a construct spanning lines (a block comment) may lose its colour.
func (r *diffRenderer) tokens(text string) []piece {
	plain := lipgloss.NewStyle().Foreground(cText)
	if r.lexer == nil {
		return []piece{{text, plain}}
	}
	it, err := r.lexer.Tokenise(nil, text)
	if err != nil {
		return []piece{{text, plain}}
	}
	var out []piece
	for _, t := range it.Tokens() {
		e := syntaxStyle.Get(t.Type)
		st := plain
		if e.Colour.IsSet() {
			st = lipgloss.NewStyle().Foreground(lipgloss.Color(e.Colour.String()))
		}
		if e.Bold == chroma.Yes {
			st = st.Bold(true)
		}
		if e.Italic == chroma.Yes {
			st = st.Italic(true)
		}
		out = append(out, piece{strings.TrimSuffix(t.Value, "\n"), st})
	}
	return out
}

// render lays a diff out w columns wide: line numbers of both sides, the
// sign, then the syntax-coloured code on a tint of its kind, the changed
// words brighter. wrap folds long lines instead of cutting them.
func (r *diffRenderer) render(d fileDiff, w int, wrap bool) []string {
	if d.binary {
		return []string{stSub.Render(" binary file — no text diff")}
	}
	if len(d.lines) == 0 {
		return []string{stSub.Render(" no textual changes (mode or rename only)")}
	}
	maxNum := 0
	for _, l := range d.lines {
		maxNum = max(maxNum, l.old, l.new)
	}
	nw := max(len(strconv.Itoa(maxNum)), 3)
	gutterW := 2*nw + 3 // "old new ±"
	textW := max(w-gutterW-1, 10)

	var out []string
	for i, l := range d.lines {
		switch l.kind {
		case '@':
			if i > 0 {
				out = append(out, "")
			}
			label := fmt.Sprintf(" ⋯ L%d ", l.new)
			ctx := ansi.Truncate(l.text, max(w-lipgloss.Width(label)-1, 0), "…")
			line := lipgloss.NewStyle().Foreground(cAccent).Background(cHunkBg).Render(label) +
				lipgloss.NewStyle().Foreground(cSub).Background(cHunkBg).Italic(true).Render(ctx)
			out = append(out, line+lipgloss.NewStyle().Background(cHunkBg).Render(strings.Repeat(" ", max(w-lipgloss.Width(line), 0))))
			continue
		case '\\':
			out = append(out, stMuted.Render(strings.Repeat(" ", gutterW)+" "+l.text))
			continue
		}

		var bg, wordBg, gutBg lipgloss.Color
		sign, signFg := " ", cMuted
		switch l.kind {
		case '+':
			bg, wordBg, gutBg, sign, signFg = cAddBg, cAddWordBg, cAddGutter, "+", cAdd
		case '-':
			bg, wordBg, gutBg, sign, signFg = cDelBg, cDelWordBg, cDelGutter, "-", cDel
		}
		num := func(n int) string {
			if n == 0 {
				return strings.Repeat(" ", nw)
			}
			return fmt.Sprintf("%*d", nw, n)
		}
		gut := lipgloss.NewStyle().Foreground(cMuted)
		sg := lipgloss.NewStyle().Foreground(signFg).Bold(true)
		if gutBg != "" {
			gut, sg = gut.Background(gutBg), sg.Background(gutBg)
		}
		gutter := gut.Render(num(l.old)+" "+num(l.new)+" ") + sg.Render(sign) + gut.Render(" ")
		blank := gut.Render(strings.Repeat(" ", gutterW-2)) + sg.Render("↪") + gut.Render(" ")

		rows := r.layout(l, bg, wordBg, textW, wrap)
		for k, row := range rows {
			g := gutter
			if k > 0 {
				g = blank
			}
			out = append(out, g+row)
		}
	}
	return out
}

// layout colours one line's code and cuts it into rows of width w (one
// row, truncated, unless wrap), each padded with the line's background.
func (r *diffRenderer) layout(l diffLine, bg, wordBg lipgloss.Color, w int, wrap bool) []string {
	var rows []string
	var cur, run strings.Builder
	col, idx := 0, 0 // column in the row, rune index in the line
	fillStyle := lipgloss.NewStyle()
	if bg != "" {
		fillStyle = fillStyle.Background(bg)
	}
	// Runes are styled in runs: one escape sequence per token, not per rune.
	var runStyle lipgloss.Style
	runKey := -1
	emit := func() {
		if run.Len() > 0 {
			cur.WriteString(runStyle.Render(run.String()))
			run.Reset()
		}
	}
	flush := func() {
		emit()
		rows = append(rows, cur.String()+fillStyle.Render(strings.Repeat(" ", max(w-col, 0))))
		cur.Reset()
		col, runKey = 0, -1
	}
	cut := false
	for ti, p := range r.tokens(l.text) {
		for _, ch := range p.text {
			cw := max(ansi.StringWidth(string(ch)), 1)
			if col+cw > w {
				if !wrap {
					cut = true
					break
				}
				flush()
			}
			inWord := l.word != [2]int{} && idx >= l.word[0] && idx < l.word[1]
			if key := 2*ti + map[bool]int{false: 0, true: 1}[inWord]; key != runKey {
				emit()
				runKey, runStyle = key, p.style
				switch {
				case inWord:
					runStyle = runStyle.Background(wordBg)
				case bg != "":
					runStyle = runStyle.Background(bg)
				}
			}
			run.WriteRune(ch)
			col += cw
			idx++
		}
		if cut {
			break
		}
	}
	if cut {
		emit()
		cur.WriteString(fillStyle.Foreground(cMuted).Render("…"))
		col++
	}
	flush()
	return rows
}

// diffHeader is the file line above a diff: status, path, counts, and the
// position in the list.
func diffHeader(f diffFile, at, n, w int) []string {
	letter := codeLetter(f.Code)
	right := statCounts(f.Add, f.Del, f.Untracked) + " " + statBar(f.Add, f.Del)
	if n > 1 {
		right += stSub.Render(fmt.Sprintf("  %d/%d", at+1, n))
	}
	path := stTitle.Render(ansi.Truncate(f.Path, max(w-lipgloss.Width(right)-5, 4), "…"))
	left := " " + stCode[letter].Bold(true).Render(string(letter)) + " " + path
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(right)-1, 1)
	return []string{left + strings.Repeat(" ", gap) + right, stLine.Render(strings.Repeat("─", w))}
}

// statCounts renders "+12 −3" (either side left out when zero).
func statCounts(add, del int, untracked bool) string {
	var parts []string
	if add > 0 || untracked {
		parts = append(parts, stOK.Render(fmt.Sprintf("+%d", add)))
	}
	if del > 0 {
		parts = append(parts, stDel.Render(fmt.Sprintf("−%d", del)))
	}
	return strings.Join(parts, " ")
}

// statBar is GitHub's five-block summary: green and red in the ratio of
// added to deleted lines, grey blocks for a change smaller than five lines.
func statBar(add, del int) string {
	g, r := barBlocks(add, del)
	return lipgloss.NewStyle().Foreground(cAdd).Render(strings.Repeat("▪", g)) +
		lipgloss.NewStyle().Foreground(cDel).Render(strings.Repeat("▪", r)) +
		stLine.Render(strings.Repeat("▪", 5-g-r))
}

// barBlocks splits min(add+del, 5) blocks between green and red, each side
// that changed getting at least one.
func barBlocks(add, del int) (green, red int) {
	total := add + del
	if total == 0 {
		return 0, 0
	}
	blocks := min(total, 5)
	green = (add*blocks + total/2) / total
	if add > 0 && green == 0 {
		green = 1
	}
	if del > 0 && green == blocks {
		green = blocks - 1
	}
	return green, blocks - green
}

// ---- viewer ----

type diffViewer struct {
	list    *diffList
	diff    fileDiff
	err     error
	lines   []string // rendered for width
	renderW int
	offset  int
	wrap    bool
	context int // lines of context around changes

	width, height int
}

func (v *diffViewer) Init() tea.Cmd { return nil }

func (v *diffViewer) load() {
	f := v.list.Files[v.list.At]
	v.diff, v.err = loadDiff(v.list.Root, v.list.Base, f, v.context)
	v.renderW, v.offset = 0, 0
}

func (v *diffViewer) render() {
	if v.renderW == v.width {
		return
	}
	v.renderW = v.width
	if v.err != nil {
		v.lines = []string{stErr.Render(" " + v.err.Error())}
		return
	}
	v.lines = newDiffRenderer(v.list.Files[v.list.At].Path).render(v.diff, max(v.width, 20), v.wrap)
}

func (v *diffViewer) bodyH() int { return max(v.height-3, 1) }

func (v *diffViewer) scroll(n int) {
	v.render()
	v.offset = min(max(v.offset+n, 0), max(len(v.lines)-v.bodyH(), 0))
}

// hunk jumps to the next (dir 1) or previous (-1) hunk header.
func (v *diffViewer) hunk(dir int) {
	v.render()
	var starts []int
	for i, l := range v.lines {
		if strings.Contains(l, " ⋯ L") {
			starts = append(starts, i)
		}
	}
	if dir > 0 {
		for _, s := range starts {
			if s > v.offset {
				v.scroll(s - v.offset)
				return
			}
		}
		return
	}
	for i := len(starts) - 1; i >= 0; i-- {
		if starts[i] < v.offset {
			v.scroll(starts[i] - v.offset)
			return
		}
	}
}

func (v *diffViewer) file(delta int) {
	next := min(max(v.list.At+delta, 0), len(v.list.Files)-1)
	if next == v.list.At {
		return
	}
	v.list.At = next
	v.load()
	v.list.tell(v.list.Files[next].Path)
}

func (v *diffViewer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		v.width, v.height = msg.Width, msg.Height
		v.renderW = 0
		v.scroll(0)
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			v.scroll(-3)
		case tea.MouseButtonWheelDown:
			v.scroll(3)
		}
	case tea.KeyMsg:
		page := v.bodyH()
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return v, tea.Quit
		case "down", "j", "enter":
			v.scroll(1)
		case "up", "k":
			v.scroll(-1)
		case " ", "f", "pgdown", "ctrl+f":
			v.scroll(page)
		case "b", "pgup", "ctrl+b":
			v.scroll(-page)
		case "d", "ctrl+d":
			v.scroll(page / 2)
		case "u", "ctrl+u":
			v.scroll(-page / 2)
		case "g", "home":
			v.scroll(-len(v.lines))
		case "G", "end":
			v.scroll(len(v.lines))
		case "n", "}":
			v.hunk(1)
		case "N", "p", "{":
			v.hunk(-1)
		case "right", "l", "tab", "]", "J":
			v.file(1)
		case "left", "h", "shift+tab", "[", "K":
			v.file(-1)
		case "w":
			v.wrap, v.renderW = !v.wrap, 0
			v.scroll(0)
		case "x": // more context around the changes, or back
			v.context = map[bool]int{true: 3, false: 25}[v.context != 3]
			v.load()
		}
	}
	return v, nil
}

func (v *diffViewer) View() string {
	v.render()
	w := max(v.width, 20)
	head := diffHeader(v.list.Files[v.list.At], v.list.At, len(v.list.Files), w)
	var b strings.Builder
	for _, l := range head {
		b.WriteString(l + "\n")
	}
	for i := v.offset; i < v.offset+v.bodyH(); i++ {
		if i < len(v.lines) {
			b.WriteString(v.lines[i])
		}
		b.WriteString("\n")
	}
	pos := ""
	if len(v.lines) > v.bodyH() {
		pos = stSub.Render(fmt.Sprintf("%d%% ", min(100, (v.offset+v.bodyH())*100/len(v.lines))))
	}
	wrap := "wrap"
	if v.wrap {
		wrap = "cut"
	}
	ctx := "context"
	if v.context != 3 {
		ctx = "less context"
	}
	help := keys("↑↓", "scroll", "n N", "hunk", "←→", "file", "w", wrap, "x", ctx, "q", "close")
	help = ansi.Truncate(help, max(w-lipgloss.Width(pos), 0), "")
	b.WriteString(help + strings.Repeat(" ", max(w-lipgloss.Width(help)-lipgloss.Width(pos), 0)) + pos)
	return b.String()
}
