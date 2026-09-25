// harness — keeps track of Claude Code sessions across projects and accounts.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/teceer/harness/internal/config"
	"github.com/teceer/harness/internal/hook"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/tmux"
)

const usage = `harness — Claude Code session control

  harness [ui]                       sidebar + sessions in the harness tmux
  harness ls [-a] [--json]           list sessions grouped by profile
  harness new [-p profile] [-n name] [-d] [dir] [-- claude args]
                                     start claude in a tmux window
  harness switch prev|next|1…9       show another session (⌘[ ⌘] ⌥1…⌥9)
  harness serve [--port 7777]        remote control over Tailscale
  harness preview <id>               print a session's recent conversation
  harness attach <id>                focus a session's tmux pane
  harness move [-d] <id>             take over a session from a terminal tab
  harness archive [-f] <id>...       stop the process, keep it resumable
  harness resume [-d] <id>           restart an archived/ended session
  harness forget <id>...             drop stopped sessions from the list
  harness gc [--idle 45m] [--dry-run]  archive sessions idle for too long
  harness sync                       mark sessions whose process died
  harness install-hooks [--dry-run] [--uninstall] [--bin path] [dir...]
                                     register hooks in Claude settings.json
  harness hook                       (called by Claude Code hooks)

<id> is a session id or any unique prefix of it.
`

func main() {
	cmd, args := "ui", []string{}
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	if cmd == "hook" {
		runHook()
		return
	}

	var err error
	switch cmd {
	case "ui":
		err = cmdUI(args)
	case "sidebar":
		err = cmdSidebar(args)
	case "switch":
		err = cmdSwitch(args)
	case "serve":
		err = cmdServe(args)
	case "preview":
		err = cmdPreview(args)
	case "ls", "list":
		err = cmdList(args)
	case "new":
		err = cmdNew(args)
	case "attach", "a":
		err = cmdAttach(args)
	case "move":
		err = cmdMove(args)
	case "archive":
		err = cmdArchive(args)
	case "resume", "r":
		err = cmdResume(args)
	case "forget":
		err = cmdForget(args)
	case "gc":
		err = cmdGC(args)
	case "sync":
		err = withStore(func(cfg *config.Config, st *store.Store) error { return sync(st) })
	case "install-hooks":
		err = cmdInstall(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
}

func withStore(fn func(*config.Config, *store.Store) error) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.StatePath())
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(cfg, st)
}

// loadConfig loads the config and points the tmux wrapper at the harness
// server config (written on demand).
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(true)
	if err != nil {
		return nil, err
	}
	tmux.ConfPath = filepath.Join(cfg.Home, "tmux.conf")
	if err := tmux.WriteConf(tmux.ConfPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

// hookDeadline caps a hook run; Claude's own timeout is 5s.
const hookDeadline = 3 * time.Second

// runHook never fails and never writes to stdout: errors go to hook.log.
func runHook() {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		cfg, err := config.Load(false)
		if err != nil {
			done <- err
			return
		}
		st, err := store.Open(cfg.StatePath())
		if err != nil {
			done <- err
			return
		}
		defer st.Close()
		done <- hook.Run(os.Stdin, cfg, st, hook.EnvFromOS())
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(hookDeadline):
		err = fmt.Errorf("timed out after %s", hookDeadline)
	}
	if err != nil {
		logHookError(err)
	}
	os.Exit(0)
}

func logHookError(err error) {
	home := config.HarnessHome()
	os.MkdirAll(home, 0o755)
	path := home + "/hook.log"
	if fi, statErr := os.Stat(path); statErr == nil && fi.Size() > 1<<20 {
		os.Rename(path, path+".1")
	}
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), err)
}
