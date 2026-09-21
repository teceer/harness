package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompleter(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"shop", "shop-api", "blog", ".hidden", "Docs"} {
		os.Mkdir(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "notes.txt"), nil, 0o644)
	os.Symlink(filepath.Join(root, "blog"), filepath.Join(root, "blog-link"))

	var c completer
	if got := c.next(root+"/sh", 1); got != root+"/shop" {
		t.Errorf("common prefix: %q", got)
	}
	if got := c.next(root+"/shop", 1); got != root+"/shop/" {
		t.Errorf("cycle 1: %q", got)
	}
	if got := c.next(root+"/shop/", 1); got != root+"/shop-api/" {
		t.Errorf("cycle 2: %q", got)
	}
	if got := c.next("", -1); got != root+"/shop/" {
		t.Errorf("cycle back: %q", got)
	}
	c.reset()
	if got := c.next(root+"/d", 1); got != root+"/Docs/" {
		t.Errorf("case-insensitive unique: %q", got)
	}
	m := dirMatches(root + "/")
	want := []string{"blog/", "blog-link/", "Docs/", "shop/", "shop-api/"}
	if len(m) != len(want) {
		t.Fatalf("matches %v", m)
	}
	for i := range want {
		if m[i] != root+"/"+want[i] {
			t.Errorf("match %d = %q, want %q (no files, no dot dirs, symlinked dir kept)", i, m[i], want[i])
		}
	}
	if m := dirMatches(root + "/."); len(m) != 1 {
		t.Errorf("dot prefix should list dot dirs: %v", m)
	}
}

func TestResolveDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"~": home, "": home, "~/code": filepath.Join(home, "code"),
		"work/x": filepath.Join(home, "work/x"), "/tmp/": "/tmp",
	} {
		if got := resolveDir(in); got != want {
			t.Errorf("resolveDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte(" \x1b[B\x1b[A\x1bOBjkx\x1b[C"))
	want := []string{" ", "down", "up", "down", "down", "up", "other", "other"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}
