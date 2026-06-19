# Signal Agent

Interact with agents via **Note to Self**.

## Overview

A service that watches a Signal **Note to Self** thread and proxies notes to an agent harness and posts the reply back in the same thread — so a phone becomes a remote for agents on a host.

Note syntax: `@route-directive /directive prompt` where directives are optional.

Route-directive is sticky so only needed when switching projects.

| directive | description
| -------   | ----
|`@project` | make project active
| `@/`      | switch to default project
| `@-`      | switch back to last project
| `@?`      | show current
| `@*`      | list projects
| `/clear`  | clear context in current project
| `/!!`     | run prompt with `--dangerously-skip-permissions`
| `/$$`     | display context headroom

Prompts are fed to the agent in a specific project folder, so context is preserved per project. Clear context with the `/clear` directive.

## Requirements

- Linux with **systemd user services** (tested on Ubuntu 24.10). The Go binary
  also cross-compiles for macOS and Windows — see [release artifacts](#install)
  — but the systemd integration here is Linux-only.
- **[signal-cli](https://github.com/AsamK/signal-cli) ≥ 0.14.5** as a native
  binary on `PATH`. (0.14.4.x had a [`getServerGuid` NPE](https://github.com/AsamK/signal-cli/issues/2059)
  that drops every incoming sealed-sender message — don't downgrade.)
- **[Claude Code](https://docs.claude.com/en/docs/claude-code) (`claude`)** on
  `PATH`, signed in. The `$$` directive also reads the OAuth token Claude
  Code writes to `~/.claude/.credentials.json`.
- **Go 1.22+** only if building from source; the release artifacts are
  static binaries with no runtime dependencies.

## Naming conventions

Everything belonging to this project — binary, source, config file, systemd
unit, state dir — uses `snake_case` (`signal_agent`), following Go's filename
convention.
Project folder names get whatever convention we use locally — the agent treats them as opaque strings.

## Getting Started

### Install

Either grab a release binary or build from source.

```bash
# release artifact (replace TAG and arch as needed)
curl -L -o ~/.local/bin/signal_agent \
  https://github.com/jaggedmountain/signal_agent/releases/download/1.0.0/signal_agent-1.0.0-linux-amd64
chmod +x ~/.local/bin/signal_agent

# or build it from source
git clone https://github.com/jaggedmountain/signal_agent.git
cd signal_agent
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/signal_agent
```

Releases are produced by GitHub Actions on each published release for
linux/darwin/windows on amd64+arm64. Each artifact has a `.sha256` sidecar.

### 1. Link this host to a Signal account

This adds the host as a *linked device* (like Signal Desktop). The phone stays
the primary device; no separate number is needed.

`signal-cli link` is a **single blocking command** with two phases: it prints a
`sgnl://linkdevice?...` URI immediately, then keeps running while it waits for
the phone to scan and finish the handshake. It exits once linking succeeds (or
times out after a couple of minutes), so keep it running and scan promptly.

Render the QR *as the URI arrives* — do not pipe straight into `qrencode`, which
waits for stdin EOF that never comes while the command blocks (it just hangs):

```bash
signal-cli link -n "agent-host" | while IFS= read -r line; do
  printf '%s\n' "$line"
  case "$line" in sgnl://*) qrencode -t ANSIUTF8 "$line";; esac
done
```

Or use two terminals: run `signal-cli link -n "agent-host"` in one, copy the
`sgnl://...` line it prints, and in the other run
`qrencode -t ANSIUTF8 'PASTE_URI'`.

Then on the phone: **Signal → Settings → Linked Devices → + (Link New Device)**
and scan the QR. The `link` command prints the account number and exits once
linking succeeds.

Verify and let it sync once:

```bash
signal-cli -a +PHONENUMBER receive          # should run without error
```

> Note: as a linked (secondary) device, signal-cli sees messages from the moment
> it was linked onward — it does not back-fill old history.

### 2. Configure

The binary self-bootstraps. Either walk through an interactive prompt:

```bash
signal_agent --env       # prompts for each value, writes the config file
```

…or let the first plain run create a template for manual edit:

```bash
signal_agent             # creates ~/.config/signal_agent.env and exits
$EDITOR ~/.config/signal_agent.env
```

Point at a non-default location with either `-c` / `--config` or the
`SIGNAL_AGENT_CONF` env var. Precedence is: env var → flag → default
(`~/.config/signal_agent.env`).

```bash
signal_agent -c /path/to/file.env --env       # interactive, custom path
SIGNAL_AGENT_CONF=/path/to/file.env signal_agent
```

Variables set in the surrounding environment win over file values, which is
handy for ad-hoc overrides (`SIGNAL_ACCOUNT=+1… signal_agent`).

### 🚨 Permissions (important)

By default Claude runs in print mode with no tool pre-approval. Because the
service is unattended it cannot answer permission prompts, so tool-using
requests will be **declined** — fine for Q&A, not for "edit this file".

Generally a project has .claude/settings.json with permissions applicable to that project.

If more convenient, set `AGENT_EXTRA_ARGS` in the env file to apply globally for just
prompts received through Signal. Either allow specific tools (safer) or skip
permissions entirely (`--dangerously-skip-permissions`).
The latter lets anything sent to Note to Self run commands and edit
files on this host, so only enable it knowing that.

Also, prefacing a note with `/!!` will include `--dangerously-skip-permissions` for just that prompt.

## 3. Try it in the foreground first

```bash
signal_agent
```

Send a Note to Self message ("what's 2+2?"). Observe log lines
and a reply appear in the thread. Ctrl-C to stop. The binary reads
`~/.config/signal_agent.env` itself — no shell sourcing needed.

## 4. Install as a systemd user service

```bash
mkdir -p ~/.config/systemd/user
cp signal_agent.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now signal_agent.service

# survive logout / run at boot without an active login session:
loginctl enable-linger "$USER"
```

Manage and inspect:

```bash
systemctl --user status signal_agent.service
systemctl --user restart signal_agent.service
journalctl --user -u signal_agent.service -f      # live logs
```

---

## Configuration reference

All settings live in `~/.config/signal_agent.env` (override with `-c
/path/to/file.env` or `SIGNAL_AGENT_CONF=/path/to/file.env`; env wins over
flag). Edit by hand or via `signal_agent --env`.

| Variable | Default | Meaning |
|---|---|---|
| `SIGNAL_ACCOUNT` | — (required) | the phone number, E.164, e.g. `+15558675309` |
| `SIGNAL_PROJECTS_ROOT` | `~/projects` | Root for `@<project>` routing |
| `SIGNAL_CLI` | `signal-cli` | Path to the signal-cli binary |
| `SIGNAL_CONFIG` | `~/.local/share/signal-cli` | signal-cli data dir, pinned to ignore ambient `XDG_DATA_HOME` |
| `AGENT_BIN` | `claude` | Path to the agent CLI |
| `AGENT_EXTRA_ARGS` | empty | Extra flags for `claude -p` (see Permissions) |
| `AGENT_TIMEOUT` | `1800` | Seconds before an agent run is killed |
| `SIGNAL_IDLE_RECYCLE_SEC` | `7200` | Recycle signal-cli after this many seconds of inbound silence; `0` disables |

## Routing notes: the active project

Every note runs in one **active project** — a directory under
`SIGNAL_PROJECTS_ROOT`. The agent persists the active project across notes
and restarts in `~/.local/state/signal_agent/active-project`, so a plain note
keeps hitting wherever we last were ("sticky").

`SIGNAL_DEFAULT_PROJECT` (env) is the project the agent starts in and the
target of `@/` / `@default`. The default must be a real folder under
`SIGNAL_PROJECTS_ROOT`; create it if it doesn't exist.

### Switching projects

| Prefix          | Effect                                                                |
| --------------- | --------------------------------------------------------------------- |
| `@<name>`       | Switch active to `<name>` and process the rest of the note there.     |
| `@/`            | Switch active to `SIGNAL_DEFAULT_PROJECT`.                            |
| `@default`      | Same as `@/`.                                                         |
| `@-`            | Swap active with previous (toggle back).                              |
| `@?`            | Report `(active: X, previous: Y)`. Ack only, no Claude call.          |
| `@*`            | List existing projects under `SIGNAL_PROJECTS_ROOT`                   |

Switches persist: a bare `@cool_jam` (no body) is acked with `(active: cool_jam)`
and the next plain note also lands in cool_jam. Unknown names (`@bogus`) don't
switch — the agent logs `[route] @bogus: not a project; staying on '<active>'`
and runs the note as-typed in the current active. Project lookup is
case-insensitive with exact-case preference, so phone autocapitalization
(`@Cool_Jam`) still resolves.

Within the active project, the agent runs `claude -p --continue` — Claude
picks up the most recent session in that cwd. The very first turn in a project
folder that's never been used with Claude is bootstrapped with a fresh UUID
(see `[route] no history in … bootstrapping …` in the log).


## Starting a fresh session: `/clear`

To drop the current context and start a new Claude session in the active
project, lead the note with `/clear`. Anything after the directive is sent as
the first prompt of the new session; bare `/clear` still gets the session file
written so the next plain note continues it.

```
/clear let's plan the next sprint
/clear                                  # fresh, send the real prompt next note
@cool_jam /clear new export format        switch to cool_jam AND fresh session
```

The directive is case-insensitive (`/CLEAR`, `/Clear`). Old sessions remain on
disk — to revisit them, run `claude --resume <uuid>` in the matching cwd.

## One-turn unsafe mode: `/!!`

By default the agent runs Claude with whatever `AGENT_EXTRA_ARGS` we set in
env (typically read-only tools). To let a single note edit files or run shell
commands without changing the env, lead it with `/!!`:

```
/!!  patch the typo in src/api/handlers.ts:142
@cool_jam /!!  bump the version in package.json and commit
```

The directive appends `--dangerously-skip-permissions` to that one `claude -p`
invocation, then reverts on the next note. Bare `/!!` with no body is a no-op.
Anything `/!!` enables persists in the resumed session, so use deliberately —
the phone is the ACL.

The sigil works with or without a leading slash (`/!!` and `!!` are the same).

## Headroom check: `@$$`, `$$`, `/$$`

All three variants report how much of our current Claude rate-limit windows
is used. Reply looks like:

```
5h: 12% (resets 4:30pm) · week: 47% (resets Sun 8:00pm)
```

The numbers come from Anthropic's `anthropic-ratelimit-unified-*` response
headers — the same data backing Claude Code's `/usage`. To get them we make a
single `POST /v1/messages` with `model=claude-haiku-4-5-20251001` and
`max_tokens=1` (a `.` as the prompt). Cost is negligible; on a 429 (throttled)
response the headers are still attached and no tokens are spent.

We authenticate with the subscription OAuth token stored in
`~/.claude/.credentials.json` (the file Claude Code writes when we log in),
sent as `Authorization: Bearer …` with `anthropic-beta: oauth-2025-04-20`.

Env overrides:

| Var | Default | Purpose |
|---|---|---|
| `SIGNAL_CLAUDE_CREDENTIALS` | `~/.claude/.credentials.json` | Where to read the OAuth token from |
| `SIGNAL_USAGE_MODEL` | `claude-haiku-4-5-20251001` | Cheap model for the throwaway call |
| `SIGNAL_USAGE_TIMEOUT` | `15` | Seconds before giving up on the API call |

Ack only — `$$` never invokes `claude` itself. Any failure (credentials
missing, network down, header format drift) degrades to a `(usage
unavailable: …)` placeholder rather than blocking note handling.

## Threat model

Run this with both eyes open. The Signal Note-to-Self thread is a
**remote-execution channel into the host** — the only thing standing between
an attacker and our shell is whatever guards our Signal account.

What's trusted:

- Anything that reaches the linked device as Note-to-Self is treated as our
  own command. The agent never authenticates the sender beyond
  "destination matches our own number" — there's no second factor.
- Our phone — physical possession + Signal app + screen lock — is the ACL.
- The OAuth token at `~/.claude/.credentials.json` is trusted for both
  `claude` invocations and the `$$` usage probe.

What's *not* assumed:

- Notes are not isolated. `/!!` runs the turn with
  `--dangerously-skip-permissions`; tools edit files, run shell commands,
  hit the network. Anything `/!!` accomplishes persists in the resumed
  session and the file system.
- The `--allowedTools` list in `AGENT_EXTRA_ARGS` is the only non-`/!!`
  guardrail. If it includes `Bash`, every note can run shell commands.
- Project-scoped `.claude/settings.json` files are loaded by `claude` and
  can broaden permissions inside that project's cwd — review them like we
  review code.

Operational hygiene:

- **Journal output contains our phone number** (`[init] account=+1…`).
  Redact `[init]` lines before pasting logs into GitHub issues or
  pastebins.
- Sessions for project-routed notes live under
  `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`. Their contents include
  prompts, tool outputs, and any secrets that flowed through them. Treat
  them like any other transcript file.
- The `signal-cli` data dir at `~/.local/share/signal-cli/` holds device
  keys and message cache. Do not back it up to untrusted storage.

If the phone is lost, SIM-swapped, or otherwise compromised, assume the host
is compromised too. The fastest containment:

1. `systemctl --user stop signal_agent.service` (stops note intake).
2. On the phone, **Settings → Linked Devices → Unlink** (stops future Signal
   server fan-out to this host even on restart).
3. Rotate `~/.claude/.credentials.json` (Anthropic login) and any tokens that
   passed through a `/!!` turn.

See [SECURITY.md](SECURITY.md) for vulnerability reporting.


## How note-to-self detection works

```
Signal (the phone)  ──linked device──▶  signal-cli (jsonRpc daemon)
                                              │
                                         signal_agent    filters note-to-self
                                              │
                                         claude -p  ──▶ reply back to Note to Self
```

Messages typed in Note to Self originate on the primary device, so the
linked host receives them as **sync transcripts**
(`envelope.syncMessage.sentMessage`) whose destination is the phone's own number. The
watcher keeps only those, ignoring normal outgoing chats to other people.
A note is processed if it has text, attachments, or both. Replies the agent
sends are not echoed back to itself, so there is no feedback loop.

### Attachments

signal-cli downloads attachments into its own store
(`$SIGNAL_CONFIG/attachments/<id>`) before the message reaches the watcher. For
every attachment, the agent passes that in-place path to Claude in the prompt —
it does not copy the file. So sending a photo or a PDF (optionally with
a caption) lets Claude open and reason about it: images are viewed directly, and
PDFs/text have their contents extracted via the Read tool. Because the paths are
stable, a file sent once can be referred to in later messages of the same
session.

For this to work, Claude must be allowed to read from the attachments directory
— ensure `AGENT_EXTRA_ARGS` permissions cover it.

## Troubleshooting

- **`User +... is not registered`** even though linking worked: signal-cli is
  reading the wrong data dir. Some shells (notably the VS Code **snap**
  terminal) set `XDG_DATA_HOME` to a confined path, so signal-cli looks there
  instead of `~/.local/share/signal-cli`. The agent pins `SIGNAL_CONFIG` to
  avoid this; for manual commands in such a terminal, pass
  `--config ~/.local/share/signal-cli` or run with `env -u XDG_DATA_HOME`.
- **No replies / `claude not found`**: confirm `PATH` in the service includes
  `~/.local/bin` (it is set in the unit) and `which claude` resolves.
- **Reset the agent's memory for a project**: send `/clear` (optionally
  prefixed with `@<project>`) — that forces a fresh session in that project's
  cwd. The previous session's transcript stays on disk at
  `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl` if we want to resume it
  manually with `claude --resume <uuid>`.
- **Re-link / unlink**: remove the device from the phone
  (Settings → Linked Devices) and re-run step 1.
