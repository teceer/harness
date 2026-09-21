package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teceer/harness/internal/config"
)

// resolveDir turns what the user typed into an absolute path: ~ expands,
// relative paths are taken from the home directory (the sidebar's own cwd
// means nothing to the user).
func resolveDir(input string) string {
	input = strings.TrimSpace(input)
	home := config.Expand("~")
	switch {
	case input == "" || input == "~":
		return home
	case strings.HasPrefix(input, "~/"):
		return filepath.Join(home, input[2:])
	case filepath.IsAbs(input):
		return filepath.Clean(input)
	}
	return filepath.Join(home, input)
}

// dirMatches lists the directories that can complete input, as it would be
// typed (keeping a leading ~), each ending in "/". The part after the last
// slash is a case-insensitive prefix; dot directories only show when the
// prefix asks for them.
func dirMatches(input string) []string {
	head, prefix := "", input
	if i := strings.LastIndex(input, "/"); i >= 0 {
		head, prefix = input[:i+1], input[i+1:]
	} else if input == "~" {
		head, prefix = "~/", ""
	}
	entries, err := os.ReadDir(resolveDir(head))
	if err != nil {
		return nil
	}
	lower := strings.ToLower(prefix)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), lower) {
			continue
		}
		if !e.IsDir() { // follow symlinks to directories
			fi, err := os.Stat(filepath.Join(resolveDir(head), name))
			if err != nil || !fi.IsDir() {
				continue
			}
		}
		out = append(out, head+name+"/")
	}
	key := func(s string) string { return strings.ToLower(strings.TrimSuffix(s, "/")) }
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

// completer implements shell-like Tab completion over dirMatches: a unique
// match is taken, a longer common prefix is filled in, otherwise repeated
// Tabs cycle through the candidates.
type completer struct {
	cycle []string // frozen candidates while cycling
	idx   int
}

// next returns the new input after Tab (delta 1) or Shift+Tab (-1).
func (c *completer) next(input string, delta int) string {
	if len(c.cycle) > 0 {
		c.idx = (c.idx + delta + len(c.cycle)) % len(c.cycle)
		return c.cycle[c.idx]
	}
	matches := dirMatches(input)
	switch {
	case len(matches) == 0:
		return input
	case len(matches) == 1:
		return matches[0]
	}
	if p := commonPrefix(matches); len(p) > len(input) {
		return p
	}
	c.cycle = matches
	c.idx = 0
	if delta < 0 {
		c.idx = len(matches) - 1
	}
	return c.cycle[c.idx]
}

// reset ends cycling (any key other than Tab).
func (c *completer) reset() { c.cycle, c.idx = nil, 0 }

// candidates is what the sidebar lists under the prompt.
func (c *completer) candidates(input string) ([]string, int) {
	if len(c.cycle) > 0 {
		return c.cycle, c.idx
	}
	return dirMatches(input), -1
}
