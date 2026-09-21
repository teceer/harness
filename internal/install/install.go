// Package install merges harness hooks into a Claude Code settings.json.
//
// Only the "hooks" key is rewritten; other top-level keys keep their order
// and raw formatting-equivalent content. Our entries are recognised by the
// Marker shell comment, so install is idempotent and uninstall is exact.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Marker = "# harness-managed"

// Events the harness listens to.
var Events = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"Notification", "Stop", "SessionEnd",
}

type field struct {
	Key   string
	Value json.RawMessage
}

// Settings is a settings.json with top-level key order preserved.
type Settings struct{ fields []field }

func parse(data []byte) (*Settings, error) {
	s := &Settings{}
	if len(bytes.TrimSpace(data)) == 0 {
		return s, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("settings.json is not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		s.fields = append(s.fields, field{Key: tok.(string), Value: raw})
	}
	return s, nil
}

func (s *Settings) get(key string) json.RawMessage {
	for _, f := range s.fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func (s *Settings) set(key string, v json.RawMessage) {
	for i, f := range s.fields {
		if f.Key == key {
			if v == nil {
				s.fields = append(s.fields[:i], s.fields[i+1:]...)
			} else {
				s.fields[i].Value = v
			}
			return
		}
	}
	if v != nil {
		s.fields = append(s.fields, field{key, v})
	}
}

func (s *Settings) marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{")
	for i, f := range s.fields {
		if i > 0 {
			b.WriteString(",")
		}
		k, _ := json.Marshal(f.Key)
		b.Write(k)
		b.WriteString(":")
		b.Write(f.Value)
	}
	b.WriteString("}")
	var out bytes.Buffer
	if err := json.Indent(&out, b.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteString("\n")
	return out.Bytes(), nil
}

type matcherGroup struct {
	Matcher *string          `json:"matcher,omitempty"`
	Hooks   []map[string]any `json:"hooks"`
}

func isOurs(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	return strings.Contains(cmd, Marker)
}

// Command is the hook command line for the given binary.
func Command(bin string) string {
	return fmt.Sprintf("'%s' hook %s", strings.ReplaceAll(bin, "'", `'\''`), Marker)
}

// Apply returns the new settings content with our hooks added (bin != "")
// or removed (bin == ""), and whether anything changed.
func Apply(data []byte, bin string) ([]byte, bool, error) {
	s, err := parse(data)
	if err != nil {
		return nil, false, err
	}
	hooks := map[string][]matcherGroup{}
	var order []string // preserve event order of the existing hooks object
	if raw := s.get("hooks"); raw != nil {
		hs, err := parse(raw)
		if err != nil {
			return nil, false, fmt.Errorf("hooks: %w", err)
		}
		for _, f := range hs.fields {
			var groups []matcherGroup
			if err := json.Unmarshal(f.Value, &groups); err != nil {
				return nil, false, fmt.Errorf("hooks.%s: %w", f.Key, err)
			}
			hooks[f.Key] = groups
			order = append(order, f.Key)
		}
	}

	// Drop our previous entries (and groups left empty by that).
	for ev, groups := range hooks {
		var kept []matcherGroup
		for _, g := range groups {
			var hs []map[string]any
			for _, h := range g.Hooks {
				if !isOurs(h) {
					hs = append(hs, h)
				}
			}
			if len(hs) > 0 {
				g.Hooks = hs
				kept = append(kept, g)
			}
		}
		hooks[ev] = kept
	}

	if bin != "" {
		for _, ev := range Events {
			if _, ok := hooks[ev]; !ok {
				order = append(order, ev)
			}
			hooks[ev] = append(hooks[ev], matcherGroup{Hooks: []map[string]any{{
				"type": "command", "command": Command(bin), "timeout": 5,
			}}})
		}
	}

	hs := &Settings{}
	for _, ev := range order {
		if len(hooks[ev]) == 0 {
			continue
		}
		raw, err := json.Marshal(hooks[ev])
		if err != nil {
			return nil, false, err
		}
		hs.fields = append(hs.fields, field{ev, raw})
	}
	if len(hs.fields) == 0 {
		s.set("hooks", nil)
	} else {
		raw, err := hs.marshal()
		if err != nil {
			return nil, false, err
		}
		s.set("hooks", bytes.TrimSpace(raw))
	}
	out, err := s.marshal()
	if err != nil {
		return nil, false, err
	}
	return out, !sameJSON(data, out), nil
}

func sameJSON(a, b []byte) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return false
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// File applies the change to <claudeDir>/settings.json, keeping a
// timestamped backup next to it. Returns whether the file changed.
func File(claudeDir, bin string, dryRun bool) (bool, error) {
	path := filepath.Join(claudeDir, "settings.json")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	out, changed, err := Apply(data, bin)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !changed || dryRun {
		return changed, nil
	}
	if data != nil {
		backup := fmt.Sprintf("%s.harness-backup-%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, data, 0o600); err != nil {
			return false, err
		}
	}
	tmp := path + ".harness-tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
