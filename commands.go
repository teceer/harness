package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/install"
	"github.com/teceer/harness/internal/proc"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

// sync marks live sessions whose process is gone as ended. Hooks cover the
// normal exit path; this catches crashes, kill -9 and closed terminals.
func sync(st *store.Store) error {
	h := liveness{panes: tmux.Panes(), claude: proc.ClaudePIDs()}
	sessions, err := st.List()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, s := range sessions {
		if !s.Live() || h.alive(s, now) {
			continue
		}
		err := st.Tx(func(tx *store.Tx) error {
			cur, ok, err := tx.Get(s.ID)
			if err != nil || !ok || !cur.Live() || h.alive(cur, now) {
				return err
			}
			cur.SetStatus(store.Ended, now)
			cur.Detail = "process gone"
			cur.TmuxPane, cur.PID = "", 0
			return tx.Put(cur)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// startGrace gives a freshly spawned session time to report SessionStart.
const startGrace = 30 * time.Second

type liveness struct {
	panes  map[string]int
	claude map[int]bool
}

// alive: every handle the session has must hold. A harness session dies
// with its pane, and a pane can outlive its claude process; before
// SessionStart reports a pid, the pane alone has to do.
func (h liveness) alive(s store.Session, now time.Time) bool {
	if s.TmuxPane != "" {
		if _, ok := h.panes[s.TmuxPane]; !ok {
			return s.Status == store.Starting && now.Sub(s.StatusSince) < startGrace
		}
	}
	if s.PID != 0 {
		return h.claude[s.PID]
	}
	if s.TmuxPane != "" {
		return true
	}
	// No handle at all: only hook events tell us it exists; trust them a while.
	return now.Sub(s.UpdatedAt) < 12*time.Hour
}

// ---- new / resume / attach ----

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	profile := fs.String("p", "", "profile (default: matched by directory)")
	name := fs.String("n", "", "window/session name")
	detach := fs.Bool("d", false, "do not attach")
	fs.Parse(args)
	rest := fs.Args()
	dir := "."
	if len(rest) > 0 && rest[0] != "--" {
		dir, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	dir, err := filepath.Abs(config.Expand(dir))
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}

	return withStore(func(cfg *config.Config, st *store.Store) error {
		pane, prof, err := newSession(cfg, st, dir, *profile, *name, rest)
		if err != nil {
			return err
		}
		fmt.Printf("started %s [%s] in %s (pane %s)\n", filepath.Base(dir), prof.Name, dir, pane)
		if *detach {
			return nil
		}
		return tmux.Attach(pane)
	})
}

// newSession starts claude in dir and records a placeholder row that the
// session's SessionStart hook will claim.
func newSession(cfg *config.Config, st *store.Store, dir, profile, name string, claudeArgs []string) (string, config.Profile, error) {
	prof, err := pickProfile(cfg, profile, dir)
	if err != nil {
		return "", prof, err
	}
	window := name
	if window == "" {
		window = filepath.Base(dir)
	}
	pane, err := spawn(cfg, window, dir, prof.Env, claudeArgs)
	if err != nil {
		return "", prof, err
	}
	now := time.Now()
	err = st.Tx(func(tx *store.Tx) error {
		return tx.Put(store.Session{
			ID: store.PendingPrefix + pane, Profile: prof.Name, ConfigDir: prof.ConfigDir(),
			Cwd: dir, Name: name, Status: store.Starting, TmuxPane: pane,
			CreatedAt: now, UpdatedAt: now, StatusSince: now,
		})
	})
	return pane, prof, err
}

func pickProfile(cfg *config.Config, name, dir string) (config.Profile, error) {
	if name != "" {
		p, ok := cfg.Profiles[name]
		if !ok {
			return p, fmt.Errorf("unknown profile %q (have: %s)", name, strings.Join(cfg.ProfileNames(), ", "))
		}
		return p, nil
	}
	p, _ := cfg.ProfileFor(dir, "")
	return p, nil
}

// spawn runs claude in a new tmux window with the profile environment set
// explicitly (tmux runs commands non-interactively, so .zshrc functions
// such as the per-directory claude() wrapper do not apply).
func spawn(cfg *config.Config, window, dir string, env map[string]string, claudeArgs []string) (string, error) {
	bin := cfg.Claude
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("claude"); err != nil {
			return "", errors.New("claude not found in PATH; set `claude` in ~/.harness/config.toml")
		}
	}
	e := map[string]string{"HARNESS_MANAGED": "1"}
	for k, v := range env {
		e[k] = v
	}
	// Claude Code drops to 256 colours whenever $TMUX is set, although this
	// server passes truecolor through; so claude must not see TMUX. The pane
	// id is handed to the hooks through HARNESS_PANE instead.
	parts := []string{
		`export HARNESS_PANE="$TMUX_PANE" HARNESS_SOCKET=` + tmux.Quote(tmux.Socket) + ";",
		"unset TMUX TMUX_PANE;",
	}
	// The default account must run WITHOUT CLAUDE_CONFIG_DIR: setting it,
	// even to ~/.claude, moves the global config from ~/.claude.json to
	// ~/.claude/.claude.json and Claude starts onboarding from scratch.
	// Unset explicitly, the tmux server may have inherited another account's.
	if d := e["CLAUDE_CONFIG_DIR"]; d == "" || filepath.Clean(d) == config.DefaultClaudeDir() {
		delete(e, "CLAUDE_CONFIG_DIR")
		parts = append(parts, "unset CLAUDE_CONFIG_DIR;")
	}
	parts = append(parts, "exec", tmux.Quote(bin))
	for _, a := range claudeArgs {
		parts = append(parts, tmux.Quote(a))
	}
	pane, err := tmux.NewWindow(cfg.TmuxSession, window, dir, e, strings.Join(parts, " "))
	if err == nil {
		tmux.FitToSlot(pane, cfg.SidebarWidth)
	}
	return pane, err
}

func cmdAttach(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: harness attach <id>")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		s, err := st.Find(args[0])
		if err != nil {
			return err
		}
		if s.TmuxPane == "" || !paneExists(s.TmuxPane) {
			if s.PID != 0 && proc.IsClaude(s.PID) {
				return fmt.Errorf("session runs outside tmux (pid %d) — switch to its terminal tab", s.PID)
			}
			return fmt.Errorf("session is not running — use: harness resume %s", short(s.ID))
		}
		return tmux.Attach(s.TmuxPane)
	})
}

func paneExists(p string) bool { _, ok := tmux.Panes()[p]; return ok }

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	detach := fs.Bool("d", false, "do not attach")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: harness resume [-d] <id>")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		if err := sync(st); err != nil {
			return err
		}
		s, err := st.Find(fs.Arg(0))
		if err != nil {
			return err
		}
		if s.Live() && s.TmuxPane != "" && !*detach {
			return tmux.Attach(s.TmuxPane)
		}
		pane, err := resumeSession(cfg, st, s)
		if err != nil {
			return err
		}
		fmt.Printf("resumed %s in pane %s\n", short(s.ID), pane)
		if *detach {
			return nil
		}
		return tmux.Attach(pane)
	})
}

// resumeSession restarts a stopped session with `claude --resume` in its
// original directory and account. Call sync first so Live() is current.
func resumeSession(cfg *config.Config, st *store.Store, s store.Session) (string, error) {
	if strings.HasPrefix(s.ID, store.PendingPrefix) {
		return "", errors.New("session never started; nothing to resume")
	}
	if s.Live() {
		return "", fmt.Errorf("session is still running (status %s)", s.Status)
	}
	if !hasTranscript(s) {
		return "", errors.New("nothing to resume: the session never exchanged a message")
	}
	env := map[string]string{}
	if p, ok := cfg.Profiles[s.Profile]; ok {
		for k, v := range p.Env {
			env[k] = v
		}
	}
	// The transcript lives under the config dir it was recorded in;
	// resuming with another account would not find it.
	env["CLAUDE_CONFIG_DIR"] = s.ConfigDir
	pane, err := spawn(cfg, windowName(s), s.Cwd, env, []string{"--resume", s.ID})
	if err != nil {
		return "", err
	}
	now := time.Now()
	err = st.Tx(func(tx *store.Tx) error {
		cur, ok, err := tx.Get(s.ID)
		if err != nil || !ok {
			return err
		}
		cur.SetStatus(store.Starting, now)
		cur.ArchivedAt, cur.TmuxPane, cur.PID, cur.Detail = time.Time{}, pane, 0, "resume"
		cur.UpdatedAt = now
		return tx.Put(cur)
	})
	return pane, err
}

// moveSession brings a session running in a plain terminal tab into the
// harness tmux: SIGTERM the claude process there, wait until it is gone,
// then `claude --resume` it here. Idle sessions only, so no turn in
// progress is cut off.
func moveSession(cfg *config.Config, st *store.Store, s store.Session) (string, error) {
	if s.TmuxPane != "" && paneExists(s.TmuxPane) {
		return "", errors.New("already running in harness")
	}
	if s.Status != store.Idle {
		return "", fmt.Errorf("session is %s — move it once it is idle", s.Status)
	}
	if !proc.IsClaude(s.PID) {
		return "", errors.New("its claude process is gone")
	}
	if !hasTranscript(s) {
		return "", errors.New("nothing to move: the session never exchanged a message")
	}
	if err := proc.Terminate(s.PID); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for proc.IsClaude(s.PID) {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("claude (pid %d) did not exit after SIGTERM", s.PID)
		}
		time.Sleep(100 * time.Millisecond)
	}
	now := time.Now()
	var cur store.Session
	err := st.Tx(func(tx *store.Tx) error {
		var ok bool
		var err error
		if cur, ok, err = tx.Get(s.ID); err != nil || !ok {
			return errors.Join(err, errors.New("session disappeared"))
		}
		cur.SetStatus(store.Ended, now)
		cur.PID, cur.TmuxPane, cur.Detail = 0, "", "moved"
		return tx.Put(cur)
	})
	if err != nil {
		return "", err
	}
	return resumeSession(cfg, st, cur)
}

func cmdMove(args []string) error {
	fs := flag.NewFlagSet("move", flag.ExitOnError)
	detach := fs.Bool("d", false, "do not attach")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: harness move [-d] <id>")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		s, err := st.Find(fs.Arg(0))
		if err != nil {
			return err
		}
		pane, err := moveSession(cfg, st, s)
		if err != nil {
			return err
		}
		fmt.Printf("moved %s into harness (pane %s)\n", short(s.ID), pane)
		if *detach {
			return nil
		}
		return tmux.Attach(pane)
	})
}

// hasTranscript: Claude writes the transcript only after the first message,
// and `claude --resume` of a session without one exits immediately.
// Unknown paths (no hook seen yet) get the benefit of the doubt.
func hasTranscript(s store.Session) bool {
	if s.TranscriptPath == "" {
		return true
	}
	_, err := os.Stat(s.TranscriptPath)
	return err == nil
}

func windowName(s store.Session) string {
	name := s.Name
	if name == "" {
		name = s.Title
	}
	if name == "" {
		name = filepath.Base(s.Cwd)
	}
	if r := []rune(name); len(r) > 24 {
		name = string(r[:24])
	}
	return name
}

// ---- archive / gc ----

func cmdArchive(args []string) error {
	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	force := fs.Bool("f", false, "archive even while working or outside tmux")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("usage: harness archive [-f] <id>...")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		var errs []error
		for _, id := range fs.Args() {
			s, err := st.Find(id)
			if err == nil {
				err = archive(st, s, *force)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				continue
			}
			fmt.Printf("archived %s %s\n", short(s.ID), display(s))
		}
		return errors.Join(errs...)
	})
}

func archive(st *store.Store, s store.Session, force bool) error {
	if s.Status == store.Archived {
		return nil
	}
	if !force && (s.Status == store.Working || s.Status == store.Starting) {
		return fmt.Errorf("session is %s (use -f to archive anyway)", s.Status)
	}
	inTmux := s.TmuxPane != "" && paneExists(s.TmuxPane)
	if !inTmux && s.PID != 0 && !force && s.Live() {
		return errors.New("session runs outside tmux; -f will SIGTERM it and close its terminal session")
	}
	now := time.Now()
	// Mark first so the SessionEnd fired by the kill does not override it.
	err := st.Tx(func(tx *store.Tx) error {
		cur, ok, err := tx.Get(s.ID)
		if err != nil || !ok {
			return err
		}
		cur.SetStatus(store.Archived, now)
		cur.ArchivedAt, cur.Detail = now, ""
		return tx.Put(cur)
	})
	if err != nil {
		return err
	}
	switch {
	case inTmux:
		err = tmux.KillPane(s.TmuxPane)
	case s.PID != 0 && proc.IsClaude(s.PID):
		err = proc.Terminate(s.PID)
	}
	if err != nil {
		return err
	}
	return st.Tx(func(tx *store.Tx) error {
		cur, ok, err := tx.Get(s.ID)
		if err != nil || !ok {
			return err
		}
		if !hasTranscript(cur) { // nothing to come back to
			return tx.Delete(cur.ID)
		}
		cur.TmuxPane, cur.PID = "", 0
		return tx.Put(cur)
	})
}

func cmdForget(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: harness forget <id>...")
	}
	return withStore(func(cfg *config.Config, st *store.Store) error {
		var errs []error
		for _, id := range args {
			s, err := st.Find(id)
			if err == nil && s.Live() {
				err = fmt.Errorf("still running (%s); archive it first", s.Status)
			}
			if err == nil {
				err = st.Tx(func(tx *store.Tx) error { return tx.Delete(s.ID) })
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
			}
		}
		return errors.Join(errs...)
	})
}

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	idleFlag := fs.String("idle", "", "archive sessions idle longer than this (default: config idle_archive)")
	dry := fs.Bool("dry-run", false, "only print what would be archived")
	fs.Parse(args)
	return withStore(func(cfg *config.Config, st *store.Store) error {
		spec := *idleFlag
		if spec == "" {
			spec = cfg.IdleArchive
		}
		idle, err := time.ParseDuration(spec)
		if err != nil {
			return fmt.Errorf("idle: %w", err)
		}
		if err := sync(st); err != nil {
			return err
		}
		sessions, err := st.List()
		if err != nil {
			return err
		}
		panes := tmux.Panes()
		for _, s := range sessions {
			// Only idle sessions harness can stop cleanly (tmux-hosted).
			if s.Status != store.Idle || time.Since(s.StatusSince) < idle {
				continue
			}
			if _, ok := panes[s.TmuxPane]; !ok {
				continue
			}
			fmt.Printf("archive %s %s (idle %s)\n", short(s.ID), display(s), ago(s.StatusSince))
			if *dry {
				continue
			}
			if err := archive(st, s, false); err != nil {
				fmt.Fprintf(os.Stderr, "  %v\n", err)
			}
		}
		return nil
	})
}

// ---- install-hooks ----

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install-hooks", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "show what would change")
	uninstall := fs.Bool("uninstall", false, "remove harness hooks")
	binFlag := fs.String("bin", "", "harness binary the hooks call (default: this executable)")
	fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	bin := ""
	if !*uninstall {
		if bin, err = hookBinary(*binFlag); err != nil {
			return err
		}
	}
	dirs := fs.Args()
	if len(dirs) == 0 {
		dirs = cfg.ClaudeDirs()
	}
	for _, d := range dirs {
		d = config.Expand(d)
		if _, err := os.Stat(d); err != nil {
			fmt.Printf("skip  %s (missing)\n", d)
			continue
		}
		changed, err := install.File(d, bin, *dry)
		if err != nil {
			return err
		}
		verb := map[bool]string{true: "update", false: "ok    "}[changed]
		if *dry && changed {
			verb = "would update"
		}
		fmt.Printf("%s %s/settings.json\n", verb, d)
	}
	if !*uninstall && !*dry {
		fmt.Println("hooks call:", install.Command(bin))
	}
	return nil
}

func hookBinary(flagVal string) (string, error) {
	bin := flagVal
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		if bin, err = filepath.EvalSymlinks(exe); err != nil {
			return "", err
		}
	}
	bin, err := filepath.Abs(config.Expand(bin))
	if err != nil {
		return "", err
	}
	tmp := os.TempDir()
	if strings.HasPrefix(bin, tmp) || strings.Contains(bin, "/go-build") {
		return "", fmt.Errorf("%s looks like a temporary build; install the binary first (make install) or pass --bin", bin)
	}
	return bin, nil
}
