package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The 8-bit stage draws every character from a canvas cache. A key that
// grows with time (the running frame counter) made a new canvas each
// animation frame, ~20 a second, until Dia ran out of GPU memory and the
// stage flickered. This runs the real sprite code from board.html in node
// over thousands of frames: the cache must stay at one canvas per frame.
func TestBoardSpriteCacheStaysBounded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page, err := webFS.ReadFile("web/board.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(page)
	from, to := strings.Index(src, "const SLOT = 40"), strings.Index(src, "const ENVELOPE")
	if from < 0 || to < from {
		t.Fatal("sprite block not found in board.html: markers 'const SLOT = 40' … 'const ENVELOPE' moved")
	}
	js := `
const document = {createElement: () => ({getContext: () => ({
  createImageData: (w, h) => ({data: new Uint8ClampedArray(w * h * 4)}), putImageData() {}})})};
` + src[from:to] + `
const roles = Object.keys(ROLE_SKIN), models = ['', ...MODELS], anims = Object.keys(ANIM);
for (let fi = 0; fi < 5000; fi++)
  for (const r of roles) for (const m of models) for (const a of anims) sprite(r, m, a, fi);
const frames = anims.reduce((n, a) => n + ANIM[a].length, 0);
console.log(spriteCache.size, roles.length * models.length * frames);
`
	path := filepath.Join(t.TempDir(), "sprite.js")
	if err := os.WriteFile(path, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		t.Fatalf("unexpected output: %s", out)
	}
	size, _ := strconv.Atoi(f[0])
	max, _ := strconv.Atoi(f[1])
	if size == 0 || size > max {
		t.Fatalf("sprite cache holds %d canvases after 5000 frames, want at most %d (one per role, model and animation frame)", size, max)
	}
}
