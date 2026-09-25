// Package config loads ~/.harness/config.toml: profiles (company/account
// groupings) and tmux settings. A default file is written on first use.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type Profile struct {
	Name  string            `toml:"-"`
	Roots []string          `toml:"roots"`
	Env   map[string]string `toml:"env"`
}

// ConfigDir is the CLAUDE_CONFIG_DIR this profile's sessions run with.
// An empty env entry means Claude's own default (~/.claude).
func (p Profile) ConfigDir() string {
	if d := p.Env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d
	}
	return DefaultClaudeDir()
}

type Config struct {
	// Claude is the claude binary; resolved from PATH when empty.
	Claude       string `toml:"claude"`
	TmuxSession  string `toml:"tmux_session"`
	IdleArchive  string `toml:"idle_archive"`
	SidebarWidth int    `toml:"sidebar_width"`
	// NotifyCommand is run (via sh) when a session starts waiting for an
	// answer or finishes a turn, with the text in $HARNESS_MESSAGE.
	NotifyCommand string `toml:"notify_command"`
	// WebURL is how `harness serve` is reached, for links in notifications.
	WebURL   string             `toml:"web_url"`
	Profiles map[string]Profile `toml:"profiles"`

	Home string `toml:"-"`
}

const defaultConfig = `# harness configuration
# claude = "/Users/you/.local/bin/claude"   # default: resolved from PATH
tmux_session = "harness"
idle_archive = "45m"                        # used by 'harness gc'
sidebar_width = 42                          # columns of the 'harness ui' sidebar

# Remote control (harness serve) and notifications. The command runs via sh
# with the text in $HARNESS_MESSAGE; it fires when a session starts waiting
# for you or finishes a turn.
# notify_command = "curl -sS -X POST ... --data-urlencode text=\"$HARNESS_MESSAGE\""
# web_url = "http://your-machine.tailnet.ts.net:7777"

# A profile groups sessions in the sidebar and decides the environment
# a new/resumed session is started with. Matching is by longest root prefix.
# Leave CLAUDE_CONFIG_DIR out for the default account (~/.claude): setting
# it explicitly makes Claude look for ~/.claude/.claude.json and re-onboard.
[profiles.personal]
roots = ["~/code"]

# A second Claude account: its own CLAUDE_CONFIG_DIR is its own login.
# [profiles.work]
# roots = ["~/work"]
# env = { CLAUDE_CONFIG_DIR = "~/.claude-work" }
`

// HarnessHome is where config, state and logs live (HARNESS_HOME overrides).
func HarnessHome() string {
	if h := os.Getenv("HARNESS_HOME"); h != "" {
		return h
	}
	return filepath.Join(userHome(), ".harness")
}

func DefaultClaudeDir() string { return filepath.Join(userHome(), ".claude") }

func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

// Expand resolves a leading ~ and cleans the path.
func Expand(p string) string {
	if p == "~" {
		return userHome()
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(userHome(), p[2:])
	}
	return filepath.Clean(p)
}

// Load reads the config, writing the default one if it does not exist yet.
// With create=false (the hook path) a missing file just yields defaults.
func Load(create bool) (*Config, error) {
	home := HarnessHome()
	path := filepath.Join(home, "config.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(defaultConfig)
		if create {
			if err := os.MkdirAll(home, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}

	var c Config
	if _, err := toml.Decode(string(data), &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Home = home
	if c.TmuxSession == "" {
		c.TmuxSession = "harness"
	}
	if c.SidebarWidth < 20 {
		c.SidebarWidth = 42
	}
	if c.IdleArchive == "" {
		c.IdleArchive = "45m"
	}
	for name, p := range c.Profiles {
		p.Name = name
		for i, r := range p.Roots {
			p.Roots[i] = Expand(r)
		}
		env := make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			if strings.HasPrefix(v, "~") {
				v = Expand(v)
			}
			env[k] = v
		}
		p.Env = env
		c.Profiles[name] = p
	}
	return &c, nil
}

func (c *Config) StatePath() string { return filepath.Join(c.Home, "state.db") }
func (c *Config) LogPath() string   { return filepath.Join(c.Home, "hook.log") }

// ProfileFor picks the profile whose root is the longest prefix of cwd;
// failing that, the only profile using the given Claude config dir.
func (c *Config) ProfileFor(cwd, claudeDir string) (Profile, bool) {
	cwd = filepath.Clean(cwd)
	best, bestLen := Profile{}, -1
	for _, p := range c.Profiles {
		for _, r := range p.Roots {
			if (cwd == r || strings.HasPrefix(cwd, r+string(filepath.Separator))) && len(r) > bestLen {
				best, bestLen = p, len(r)
			}
		}
	}
	if bestLen >= 0 {
		return best, true
	}
	if claudeDir != "" {
		var match []Profile
		for _, p := range c.Profiles {
			if p.ConfigDir() == filepath.Clean(claudeDir) {
				match = append(match, p)
			}
		}
		if len(match) == 1 {
			return match[0], true
		}
	}
	return Profile{Name: "other"}, false
}

func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ClaudeDirs lists every distinct CLAUDE_CONFIG_DIR in use (for hook install).
func (c *Config) ClaudeDirs() []string {
	seen := map[string]bool{DefaultClaudeDir(): true}
	dirs := []string{DefaultClaudeDir()}
	for _, name := range c.ProfileNames() {
		d := c.Profiles[name].ConfigDir()
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}
