package config

import (
	"os"
	"path/filepath"
	"testing"
)

const testConfig = `
[profiles.personal]
roots = ["~/code"]

[profiles.work]
roots = ["~/work"]
env = { CLAUDE_CONFIG_DIR = "~/.claude-work" }

[profiles.client]
roots = ["~/client", "~/cl"]
`

func TestProfileFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HARNESS_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o644)
	c, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ cwd, dir, want string }{
		{Expand("~/work/api"), "", "work"},
		{Expand("~/code/harness"), "", "personal"},
		{Expand("~/cl/x"), "", "client"},
		{Expand("~/workshop"), "", "other"}, // prefix must end at a path boundary
		{"/tmp", Expand("~/.claude-work"), "work"},
		{"/tmp", DefaultClaudeDir(), "other"}, // shared by two profiles → ambiguous
	}
	for _, tc := range cases {
		if got, _ := c.ProfileFor(filepath.Clean(tc.cwd), tc.dir); got.Name != tc.want {
			t.Errorf("ProfileFor(%s, %s) = %s, want %s", tc.cwd, tc.dir, got.Name, tc.want)
		}
	}
}

func TestDefaultConfigLoads(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	c, err := Load(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Profiles["personal"]; !ok || len(c.Profiles) != 1 {
		t.Errorf("default profiles = %v", c.ProfileNames())
	}
}

func TestRemotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HARNESS_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig+`
[remotes.box]
url = "https://box.example.ts.net/"

[remotes.lab]
url = "http://10.0.0.5:7777"
token = " own-token "

[remotes."bad:name"]
url = "https://x.example"

[remotes.nourl]
url = "box.example"
`), 0o644)
	c, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RemoteNames(); len(got) != 4 || got[1] != "box" || got[2] != "lab" {
		t.Fatalf("RemoteNames = %v", got)
	}
	box := c.Remotes["box"]
	if box.Name != "box" || box.URL != "https://box.example.ts.net" || box.Validate() != nil {
		t.Errorf("box = %+v, %v", box, box.Validate())
	}
	for _, name := range []string{"bad:name", "nourl"} {
		if c.Remotes[name].Validate() == nil {
			t.Errorf("remote %q should not validate", name)
		}
	}
	// its own token wins; otherwise the local web-token, which must exist
	if tok, err := c.RemoteToken(c.Remotes["lab"]); err != nil || tok != "own-token" {
		t.Errorf("lab token = %q, %v", tok, err)
	}
	if _, err := c.RemoteToken(box); err == nil {
		t.Error("no web-token yet: want an error")
	}
	os.WriteFile(filepath.Join(home, "web-token"), []byte("shared-token-0123456789\n"), 0o600)
	if tok, err := c.RemoteToken(box); err != nil || tok != "shared-token-0123456789" {
		t.Errorf("box token = %q, %v", tok, err)
	}
	if len(c.Profiles) != 3 {
		t.Errorf("remotes leaked into profiles: %v", c.ProfileNames())
	}
}
