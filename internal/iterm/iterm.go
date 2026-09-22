// Package iterm gives the harness tab its ⌘ shortcuts.
//
// Terminal programs never see ⌘ chords, and iTerm2 uses ⌘[ / ⌘] for its own
// pane switching. A dynamic profile "Harness" (inheriting everything from
// the user's profile) maps ⌘[ ⌘] ⌥⇥ ⌘⇧A ⌘⇧N ⌥1…⌥9 to private escape sequences that the
// harness tmux binds. `harness ui` switches its tab to that profile while
// attached, so iTerm2 behaves as usual everywhere else.
package iterm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// parent as the profile it inherits colours, fonts and keys from. changed
// reports that the file was (re)written: iTerm2 loads it asynchronously, so
// the profile may not be selectable for a moment.
func EnsureProfile(parent string) (changed bool, err error) {
	path, err := profilePath()
	if err != nil {
		return false, err
	}
	doc := map[string]any{"Profiles": []map[string]any{{
		"Name":                        ProfileName,
		"Guid":                        "harness-dynamic-profile",
		"Dynamic Profile Parent Name": parent,
		"Keyboard Map":                mergedKeyMap(parentKeyMap(prefs(), parent)),
	}}}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, data, 0o644)
}

// mergedKeyMap puts the harness keys over the parent's own mappings. A
// dynamic profile inherits its parent key by key, and "Keyboard Map" is one
// key: without the parent's entries the Harness tab would lose e.g. the
// Natural Text Editing preset (⌥← ⌥→ by word, ⌘← line start).
func mergedKeyMap(parent map[string]any) map[string]any {
	m := make(map[string]any, len(parent)+len(keyMap))
	maps.Copy(m, parent)
	for k, v := range keyMap {
		m[k] = v
	}
	return m
}

// prefs is iTerm2's preferences as an XML plist. `defaults export` goes
// through cfprefsd, so it sees settings iTerm2 has not flushed to disk yet.
func prefs() []byte {
	out, _ := exec.Command("defaults", "export", "com.googlecode.iterm2", "-").Output()
	return out
}

// parentKeyMap is the Keyboard Map of the profile named name in the iTerm2
// preferences plist, or of the default profile when there is none by that
// name (iTerm2 falls back to it for an unknown parent too). nil if neither
// is found.
func parentKeyMap(plist []byte, name string) map[string]any {
	if len(plist) == 0 {
		return nil
	}
	defGuid := plistValue(plist, "Default Bookmark Guid", "raw")
	fallback := -1
	for i := 0; ; i++ {
		guid := plistValue(plist, fmt.Sprintf("New Bookmarks.%d.Guid", i), "raw")
		if guid == "" {
			break
		}
		if plistValue(plist, fmt.Sprintf("New Bookmarks.%d.Name", i), "raw") == name {
			return keyMapAt(plist, i)
		}
		if guid == defGuid {
			fallback = i
		}
	}
	if fallback < 0 {
		return nil
	}
	return keyMapAt(plist, fallback)
}

func keyMapAt(plist []byte, i int) map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(plistValue(plist, fmt.Sprintf("New Bookmarks.%d.Keyboard Map", i), "json")), &m)
	return m
}

// plistValue extracts keypath from plist with plutil; "" if absent.
func plistValue(plist []byte, keypath, format string) string {
	cmd := exec.Command("plutil", "-extract", keypath, format, "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(plist)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
