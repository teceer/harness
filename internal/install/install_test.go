package install

import (
	"encoding/json"
	"strings"
	"testing"
)

const withForeignHook = `{
  "permissions": {"defaultMode": "bypassPermissions"},
  "model": "opus[1m]",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/x/session-start.sh", "timeout": 15, "statusMessage": "kb"}]}
    ]
  },
  "language": "English"
}`

func TestApplyKeepsForeignHooksAndOrder(t *testing.T) {
	out, changed, err := Apply([]byte(withForeignHook), "/bin/harness")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !(strings.Index(s, `"permissions"`) < strings.Index(s, `"model"`) &&
		strings.Index(s, `"model"`) < strings.Index(s, `"hooks"`) &&
		strings.Index(s, `"hooks"`) < strings.Index(s, `"language"`)) {
		t.Errorf("top-level key order changed:\n%s", s)
	}
	var parsed struct {
		Hooks map[string][]matcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	ss := parsed.Hooks["SessionStart"]
	if len(ss) != 2 || ss[0].Hooks[0]["command"] != "/x/session-start.sh" {
		t.Errorf("foreign SessionStart hook not preserved first: %+v", ss)
	}
	for _, ev := range Events {
		if len(parsed.Hooks[ev]) == 0 {
			t.Errorf("missing %s", ev)
		}
	}

	again, changed, err := Apply(out, "/bin/harness")
	if err != nil || changed || string(again) != string(out) {
		t.Errorf("second apply not idempotent: changed=%v err=%v", changed, err)
	}

	removed, changed, err := Apply(out, "")
	if err != nil || !changed {
		t.Fatalf("uninstall changed=%v err=%v", changed, err)
	}
	if strings.Contains(string(removed), Marker) || !strings.Contains(string(removed), "session-start.sh") {
		t.Errorf("uninstall wrong:\n%s", removed)
	}
}

func TestApplyEmptyAndUninstallRemovesHooksKey(t *testing.T) {
	out, _, err := Apply(nil, "/bin/harness")
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := Apply(out, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(back), "hooks") {
		t.Errorf("expected hooks key gone, got %s", back)
	}
}

func TestCommandQuotesPath(t *testing.T) {
	got := Command("/Users/a b/harness")
	if got != `'/Users/a b/harness' hook # harness-managed` {
		t.Errorf("got %s", got)
	}
}
