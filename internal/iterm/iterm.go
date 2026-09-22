// Package iterm gives the harness tab its ⌘ shortcuts.
//
// Terminal programs never see ⌘ chords, and iTerm2 uses ⌘[ / ⌘] for its own
// pane switching. A dynamic profile "Harness" (inheriting everything from
// the user's profile) maps ⌘[ ⌘] ⌥⇥ ⌘⇧A ⌘⇧N ⌥1…⌥9 to private escape sequences that the
// harness tmux binds. `harness ui` switches its tab to that profile while
// attached, so iTerm2 behaves as usual everywhere else.
package iterm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const ProfileName = "Harness"

// Keyboard Map keys are "0x<char>-0x<modifiers>-0x<keycode>"; 0x100000 is ⌘.
// Action 10 sends ESC followed by Text (the tmux.Seq* sequences).
var keyMap = func() map[string]map[string]any {
	m := map[string]map[string]any{
		"0x5b-0x100000-0x21": {"Action": 10, "Text": "[1000~"}, // ⌘[  → harness switch prev
		"0x5d-0x100000-0x1e": {"Action": 10, "Text": "[1001~"}, // ⌘]  → harness switch next
		"0x9-0x80000-0x30":   {"Action": 10, "Text": "[1002~"}, // ⌥⇥  → sidebar ⇄ session
		"0x41-0x120000-0x0":  {"Action": 10, "Text": "[1003~"}, // ⌘⇧A → Sessions ⇄ Archived (0x120000 = ⌘⇧)
		"0x4e-0x120000-0x2d": {"Action": 10, "Text": "[1004~"}, // ⌘⇧N → new session
	}
	// ⌥1…⌥9 → harness switch n (macOS virtual key codes of the digit row).
	// Not ⌘: iTerm2 takes ⌘1…⌘9 for its tabs before it looks at profile
	// key mappings. 0x80000 is ⌥; only ⌥+digit is taken, so ⌥ still types
	// accented letters, and outside the harness tab ⌥+digit is untouched.
	keycodes := [...]int{0x12, 0x13, 0x14, 0x15, 0x17, 0x16, 0x1a, 0x1c, 0x19}
	for i, kc := range keycodes {
		n := i + 1
		m[fmt.Sprintf("0x%x-0x80000-0x%x", '0'+n, kc)] = map[string]any{"Action": 10, "Text": fmt.Sprintf("[10%d~", 10+n)}
	}
	return m
}()

// Active reports whether we run directly in iTerm2 (not nested in tmux).
func Active() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app" && os.Getenv("TMUX") == ""
}

func profilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "iTerm2", "DynamicProfiles", "harness.json"), nil
}

// EnsureProfile writes the dynamic profile (iTerm2 picks it up live) with
// parent as the profile it inherits colours, fonts and keys from.
func EnsureProfile(parent string) error {
	path, err := profilePath()
	if err != nil {
		return err
	}
	doc := map[string]any{"Profiles": []map[string]any{{
		"Name":                        ProfileName,
		"Guid":                        "harness-dynamic-profile",
		"Dynamic Profile Parent Name": parent,
		"Keyboard Map":                keyMap,
	}}}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// SetProfile switches the current iTerm2 session to the named profile.
func SetProfile(w io.Writer, name string) {
	fmt.Fprintf(w, "\x1b]1337;SetProfile=%s\x07", name)
}

// CurrentProfile is the profile this tab was opened with.
func CurrentProfile() string {
	if p := os.Getenv("ITERM_PROFILE"); p != "" && p != ProfileName {
		return p
	}
	return "Default"
}
