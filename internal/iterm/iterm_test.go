package iterm

import "testing"

const prefsPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Default Bookmark Guid</key><string>g-def</string>
	<key>New Bookmarks</key>
	<array>
		<dict>
			<key>Name</key><string>Work</string>
			<key>Guid</key><string>g-work</string>
			<key>Keyboard Map</key>
			<dict>
				<key>0xf702-0x280000-0x7b</key>
				<dict><key>Action</key><integer>10</integer><key>Text</key><string>b</string></dict>
				<key>0x5b-0x100000-0x21</key>
				<dict><key>Action</key><integer>0</integer><key>Text</key><string>x</string></dict>
			</dict>
		</dict>
		<dict>
			<key>Name</key><string>Main</string>
			<key>Guid</key><string>g-def</string>
			<key>Keyboard Map</key>
			<dict>
				<key>0xf703-0x280000-0x7c</key>
				<dict><key>Action</key><integer>10</integer><key>Text</key><string>f</string></dict>
			</dict>
		</dict>
	</array>
</dict>
</plist>`

func TestParentKeyMap(t *testing.T) {
	if m := parentKeyMap([]byte(prefsPlist), "Work"); len(m) != 2 || m["0xf702-0x280000-0x7b"] == nil {
		t.Errorf("by name: %v", m)
	}
	// Unknown parent: iTerm2 uses the default profile, so do we.
	if m := parentKeyMap([]byte(prefsPlist), "Default"); len(m) != 1 || m["0xf703-0x280000-0x7c"] == nil {
		t.Errorf("fallback: %v", m)
	}
	if m := parentKeyMap(nil, "Work"); m != nil {
		t.Errorf("no prefs: %v", m)
	}
}

func TestMergedKeyMapKeepsParentAndOverrides(t *testing.T) {
	m := mergedKeyMap(parentKeyMap([]byte(prefsPlist), "Work"))
	if m["0xf702-0x280000-0x7b"] == nil {
		t.Error("parent mapping lost")
	}
	if got := m["0x5b-0x100000-0x21"].(map[string]any)["Text"]; got != "[1000~" {
		t.Errorf("⌘[ = %v, want the harness sequence", got)
	}
	if len(m) != len(keyMap)+1 {
		t.Errorf("len = %d, want %d", len(m), len(keyMap)+1)
	}
}
