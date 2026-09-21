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
