// Package proc finds and probes the claude process behind a hook call.
package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ClaudeAncestor walks up from our parent (hooks run as claude → shell →
// harness) and returns the first claude session process, or 0.
func ClaudeAncestor() int {
	pid := os.Getppid()
	for range 5 {
		if pid <= 1 {
			return 0
		}
		ppid, args, ok := psInfo(pid)
		if !ok {
			return 0
		}
		if isDaemon(args) {
			return 0 // a hook run by Claude's background daemon: no session pid
		}
		if IsClaudeArgs(args) {
			return pid
		}
		pid = ppid
	}
	return 0
}

func psInfo(pid int) (ppid int, args string, ok bool) {
	out, err := exec.Command("ps", "-o", "ppid=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", false
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err = strconv.Atoi(f[0])
	return ppid, strings.Join(f[1:], " "), err == nil
}

// IsClaudeArgs reports whether a command line is an interactive claude
// session. `claude daemon run …` shares the binary but outlives sessions,
// so it must never stand in for one.
func IsClaudeArgs(args string) bool {
	f := strings.Fields(args)
	return len(f) > 0 && filepath.Base(f[0]) == "claude" && !isDaemon(args)
}

func isDaemon(args string) bool {
	f := strings.Fields(args)
	return len(f) > 1 && filepath.Base(f[0]) == "claude" && f[1] == "daemon"
}

// IsClaude reports whether pid is alive and still a claude process
// (guards against pid reuse after the session exited).
func IsClaude(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	_, args, ok := psInfo(pid)
	return ok && IsClaudeArgs(args)
}

// ClaudePIDs returns the pids of all running claude processes (one ps call).
func ClaudePIDs() map[int]bool {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	pids := map[int]bool{}
	if err != nil {
		return pids
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !IsClaudeArgs(strings.Join(f[1:], " ")) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			pids[pid] = true
		}
	}
	return pids
}

func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
