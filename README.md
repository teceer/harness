# harness

Tracks Claude Code sessions across projects and accounts; archives idle ones
to free RAM and resumes them on demand.

```
tmux ── hosts every claude process (harness can crash without killing agents)
hooks ─ Claude Code calls `harness hook` on every event → ~/.harness/state.db
CLI  ── reads state.db, drives tmux
```

No daemon: hooks write, the CLI reads. A hook run takes <10 ms, never prints
and always exits 0, so a broken harness cannot break Claude.

## Requirements

- macOS (process and key-state probes use macOS tools/APIs)
- [Claude Code](https://claude.com/claude-code)
- tmux ≥ 3.3, Go ≥ 1.26 and a C compiler (Xcode Command Line Tools) to build
- iTerm2 for the ⌘/⌥ shortcuts (everything else works in any terminal)

## Install

```sh
make hooks        # builds, installs ~/bin/harness, registers hooks in every
                  # CLAUDE_CONFIG_DIR from ~/.harness/config.toml
harness install-hooks --uninstall   # removes only harness entries
```

`settings.json` is backed up (`settings.json.harness-backup-*`) before every
change. Other hooks and key order are preserved.

## Sidebar

```sh
harness            # = harness ui: attach to the harness tmux server
```

```
┌─ Sessions 4 │ Archived 1 ─────── ◆1 ┐┌──────────────────────────┐
│ ACTIVE ───────────────────────── 3  ││                          │
│  personal                           ││  the selected claude     │
│▌1 ● blog                 Bash 1m    ││  session                 │
│     > fix the RSS feed              ││                          │
│  work                               ││                          │
│ 2 ○ api                        3m   ││                          │
│ OUTSIDE HARNESS ──────────────── 1  ││                          │
│  work                               ││                          │
│   ● shop · Checkout flow     Bash   ││                          │
└─────────────────────────────────────┘└──────────────────────────┘
```

**Sessions** lists running sessions: *active* ones live in the harness tmux,
*outside harness* ones in plain terminal tabs. **Archived** holds stopped,
resumable sessions (archived, and ones that ended in the last day).

| key | action |
|---|---|
| `⌥1` … `⌥9` | anywhere: show active session number n (numbers in the sidebar) |
| `⌘[` / `⌘]` | anywhere: show the previous / next active session |
| `⌥⇥` | anywhere: toggle between the sidebar and the session |
| `⌘⇧A` | in the sidebar: toggle Sessions ⇄ Archived; in a session: jump to the sidebar's Archived tab |
| `⏎` | show the session on the right and focus it (resumes a stopped one; for a session in a plain terminal tab asks `[Y/n]` to move it here, ⏎ confirms) |
| `space` | **hold** to peek at the selected session's recent conversation, in either tab (popup over the right pane, closes when you let go; nothing is resumed or switched). While holding, `↑` `↓` (or `k` `j`) peek at the neighbouring sessions and the sidebar selection follows |
| `Tab` | move into the session shown on the right |
| `1` … `9` | in the sidebar: same as `⌥1` … `⌥9` |
| `←` `→` | switch tabs (or click them) |
| `⌘⇧N` | anywhere: new session — directory prompt in the sidebar with `Tab` completion (`Tab`/`↓` next, `⇧Tab`/`↑` previous; relative paths start at `~`); profile from the path |
| `a` | archive (`a` twice / `A` when it is working) |
| `e` | rename · `x` forget a stopped session · `.` all ended ones |
| `q` | detach (everything keeps running) · `ctrl+c` quit the sidebar |

### ⌘ shortcuts

Terminal programs never receive ⌘ chords, and iTerm2 uses `⌘[` `⌘]` for its
own pane switching. `harness` therefore writes an iTerm2 dynamic profile
(`~/Library/Application Support/iTerm2/DynamicProfiles/harness.json`)
named *Harness*: it inherits everything from the profile you started in
(its key mappings included) and on top maps `⌘[` `⌘]` `⌥⇥` `⌘⇧A` `⌘⇧N`
`⌥1`…`⌥9` to private escape sequences the harness tmux binds (`user-keys`).
The tab switches to that profile while attached and back when you detach,
so every other iTerm2 tab behaves as before.

Session numbers use ⌥, not ⌘: iTerm2 handles ⌘1…⌘9 (*Settings → Keys →
Navigation Shortcuts → select a tab*) before it consults profile key
mappings, so a profile cannot take them over. Only ⌥+digit is mapped, so ⌥
keeps typing accented letters.

Switching swaps panes in one tmux call, and background windows are kept at
the size of the slot next to the sidebar: a session never sees a resize
when it is shown, so nothing reflows or flickers. The sidebar watches the
state database and `ui.bump` (touched by `harness switch`) every 100 ms and
redraws as soon as either changes.

A tmux binding catches its key before Claude sees it, which is why the
shortcuts avoid plain Tab / Shift+Tab (Claude's completion and mode
cycling; ⌥Tab is free) and ⌥ + letters (accented characters on many
keyboard layouts).

Harness uses its own tmux server (`tmux -L harness`, config in
`~/.harness/tmux.conf`; put your additions in `tmux.local.conf`), so it
never touches your own tmux. The sidebar restarts itself if it crashes.

Claude runs with `TMUX` hidden: Claude Code falls back to 256 colours when
it sees `$TMUX`, although this server passes truecolor through. The pane id
reaches the hooks as `HARNESS_PANE` instead.

## CLI

```sh
harness ls                 # grouped by profile: status, age, last message
harness new -n login ~/work/api   # claude in tmux, work account
harness archive 184c       # kill the process, keep the session
harness resume 184c        # claude --resume in the same cwd + account
harness gc --dry-run       # what idle_archive would stop
```

Statuses: `working` (tool name shown), `waiting` (permission prompt),
`idle`, `starting`, `archived`, `ended`.

Sessions started outside harness (plain iTerm tabs) are tracked too, as soon
as they fire a hook; archiving them needs `-f` (SIGTERM), `harness move`
takes them over into the harness tmux.

## Remote control (phone)

```sh
harness serve          # prints the URL, token included
```

A small web app — the same session list (grouped by profile, with a filter
per profile), each session on its own page, a reply box, archive / resume
and starting a session in a known directory.

Each session page has two views. **conversation** streams the transcript:
Claude Code appends per block (a paragraph, a tool call), so answers show
up about a second after they are written, images pasted into the session
included. **live** mirrors the tmux pane a few times a second, colours and
all — the spinner and tool output as they happen, which is as close to
token-by-token as a transcript-based tool can get. 📎 sends a photo or
screenshot: harness saves it under `~/.harness/uploads` and hands the
session its path, which is how Claude reads images. It is meant for a phone: answer a permission prompt from the
sofa instead of leaving agents stuck until you are back.

It listens on **loopback only**; `tailscale serve` publishes it on the
tailnet (`http://<machine>.<tailnet>.ts.net:7777`) and proxies to
127.0.0.1. That keeps the listener off every real interface, and inbound
connections are accepted by Tailscale itself — the macOS firewall silently
drops them for an unsigned binary like this one, which is why binding the
tailnet address directly does not work. Use the MagicDNS name, not the
100.x address. `--no-tailscale` skips publishing.

Every API call carries the token from
`~/.harness/web-token` (`?t=…` once, then kept in the browser). Remote
actions can only type into existing sessions or press one of a few allowed
keys (`Enter`, `Escape`, `1`, `2`, `y`, `n`, …): there is no endpoint that
runs a command. Every action is logged with the caller's address.

This is still remote control of a machine that runs Claude Code, often with
relaxed permissions. Do not put it behind a public tunnel; if you must,
require an identity check (e.g. ngrok's OAuth), not just the token.

Notifications tell you when a session starts waiting for you or finishes a
turn. `notify_command` runs via `sh` with the text in `$HARNESS_MESSAGE`
(and `$HARNESS_SESSION`, `$HARNESS_STATUS`), so any channel works:

```toml
notify_command = "curl -sS -X POST https://api.telegram.org/bot$TOKEN/sendMessage -d chat_id=$CHAT --data-urlencode text=\"$HARNESS_MESSAGE\""
web_url = "http://your-mac.tailnet.ts.net:7777"
```

Both are top-level keys: in TOML they must come **before** the first
`[profiles.…]` table, or they end up inside it.

## Profiles

`~/.harness/config.toml` (written on first run) maps directory roots to an
environment. Sessions are grouped by profile in the sidebar, and new or
resumed sessions start with the profile's environment — e.g. two Claude
accounts, each with its own login:

```toml
[profiles.personal]
roots = ["~/code"]

[profiles.work]
roots = ["~/work"]
env = { CLAUDE_CONFIG_DIR = "~/.claude-work" }
```

Leave `CLAUDE_CONFIG_DIR` out for the default account: setting it to
`~/.claude` makes Claude read `~/.claude/.claude.json` instead of
`~/.claude.json` and start onboarding from scratch. Hooks are installed into
every config dir the profiles use (`harness install-hooks`).

## Debug

- `HARNESS_TMUX_SOCKET=x HARNESS_HOME=/tmp/h harness …` — a throwaway
  server and state, for testing without touching real sessions
- `HARNESS_UI_DEBUG=/tmp/ui.log` — log every sidebar input event
- `~/.harness/hook.log` — hook errors (rotated at 1 MB)
- `HARNESS_DEBUG=1` — raw hook payloads to `~/.harness/hook-debug.jsonl`

Terminals report no key releases (and tmux would not pass them on), so the
peek asks macOS for the physical key state through `harness-keywait`, a tiny
C helper installed next to `harness` (kept out of the main binary so hooks
start fast): the popup closes within ~10 ms of letting go. If the helper is
missing or macOS does not expose the state, it falls back to "held while
auto-repeat keeps arriving" (timed from *InitialKeyRepeat* / *KeyRepeat*,
closes ~0.2 s after release). A tap shows it for about a second. Pressing
an arrow stops space's auto-repeat, so in that fallback the peek closes
shortly after arrows are released; with the helper it stays until space
itself is let go.
`harness preview <id>` prints the same view.

## License

MIT — see [LICENSE](LICENSE).

## Known limits

- Interrupting Claude (Esc) fires no hook, so the session shows `working`
  until its next event.
- A session that never exchanged a message has no transcript; archiving
  it just closes it.
- Moving a terminal-tab session into harness (`⏎` → `y`, or
  `harness move <id>`) needs it idle; its tab is left at the shell prompt.
