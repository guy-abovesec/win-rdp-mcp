# win-rdp-mcp

Control a Windows machine over RDP, exposed as [Model Context Protocol](https://modelcontextprotocol.io/)
tools — screenshots, mouse and keyboard, PowerShell, files, processes, services,
the registry, scheduled tasks, the event log and network checks.

**You give it only RDP credentials.** It connects, drives the desktop directly,
pushes a small agent into the session for the system internals, and exposes the
whole thing to your AI client. Nothing to pre-install on the target — no SMB, no
WinRM, no open port beyond RDP.

Originally a Go port of [dddabtc/winremote-mcp](https://github.com/dddabtc/winremote-mcp);
it has since grown the RDP controller so the target needs no agent installed.

---

## How it works

Two halves, one binary:

```
  your machine (Linux)                          the Windows target
┌──────────────────────────────┐              ┌──────────────────────────┐
│ win-rdp-mcp control          │              │                          │
│  the MCP server your AI uses │              │                          │
│                              │── RDP/3389 ──▶│  live desktop session    │
│  • xfreerdp holds a session  │   screenshots │  (logged in for you)     │
│    in a headless Xvfb        │   + input     │                          │
│  • screenshots + mouse/keys  │              │                          │
│    driven locally            │              │                          │
│                              │── drive ─────▶│  win-rdp-mcp.exe (agent) │
│  • pushes the agent over the │   redirection │   pushed + run in-session│
│    redirected drive, talks   │◀── files ────▶│   window tree, shell,    │
│    to it via files on it     │   over RDP    │   registry, files, ...   │
└──────────────────────────────┘              └──────────────────────────┘
```

- The **desktop tier** — `Snapshot`, `Click`, `Type`, `Move`, `Scroll`,
  `Shortcut` — is driven straight over RDP with **no footprint on the target**.
- The **system tier** — window enumeration with coordinates, `Shell`, files,
  registry, services, tasks, event log, OCR — is served by an agent the
  controller **pushes into the RDP session over drive redirection** and talks to
  through files on that same redirected drive. Still nothing but RDP on the wire.

The agent can also run standalone on the target (stdio or HTTP) if you'd rather
install it — see [Running the agent directly](#running-the-agent-directly).

---

## Quickstart

On your Linux machine:

```sh
# with Nix — bundles xfreerdp/xdotool/imagemagick/xvfb, nothing else to install
nix run github:stubbedev/win-rdp-mcp -- control \
  --target 192.168.1.50 --user administrator

# the password comes from the environment (kept out of your shell history/args)
export WIN_RDP_TARGET_PASS='...'
```

That starts an MCP server on stdio. Point your client at it (below). The RDP
session opens on the first tool call, and reopens on its own if it drops —
Windows gives a user one session, so another client logging in as the same user
takes it. The first system-tool call waits a few seconds while the agent
bootstraps; the desktop tools work immediately.

### Requirements (controller host)

The controller shells out to `xfreerdp` (FreeRDP 3 — `xfreerdp3` is used when
present, which is what Debian and Ubuntu call it), `xdotool`, `import`
(ImageMagick), `Xvfb` and `xdpyinfo`. The Nix package and dev shell bundle them.
Installing another way, get them from your package manager, e.g. on
Debian/Ubuntu:

```sh
sudo apt install freerdp3-x11 xdotool imagemagick xvfb x11-utils
```

The target account must be able to log in over RDP. That's it — no admin share,
no WinRM, no firewall change.

---

## Connect an MCP client

The controller *is* the MCP server; your client launches it. It runs on your
machine, not the target.

### Claude Code / Claude Desktop

```json
{
  "mcpServers": {
    "windows": {
      "command": "win-rdp-mcp",
      "args": ["control", "--target", "192.168.1.50", "--user", "administrator"],
      "env": { "WIN_RDP_TARGET_PASS": "your-password" }
    }
  }
}
```

Or, without installing anything, use the Nix runner as the command:

```json
{
  "mcpServers": {
    "windows": {
      "command": "nix",
      "args": ["run", "github:stubbedev/win-rdp-mcp", "--", "control",
               "--target", "192.168.1.50", "--user", "administrator"],
      "env": { "WIN_RDP_TARGET_PASS": "your-password" }
    }
  }
}
```

To keep the password out of the client config, drop the `env` and point
`--pass-file` at a `chmod 600` file instead:

```json
"args": ["control", "--target", "192.168.1.50", "--user", "administrator",
         "--pass-file", "/home/you/.config/win-rdp-mcp/target.pass"]
```

In Claude Code you can do all of this in one line:

```sh
claude mcp add win-rdp -s user -- \
  win-rdp-mcp control --target 192.168.1.50 --user administrator \
  --pass-file ~/.config/win-rdp-mcp/target.pass
```

---

## Install

### Nix (recommended — bundles the runtime tools)

```sh
nix run github:stubbedev/win-rdp-mcp -- control --target ... --user ...
nix profile install github:stubbedev/win-rdp-mcp
```

Builds are pushed to a public binary cache. Add `--accept-flake-config`, or put
these in your `nix.conf`:

```
extra-substituters = https://nix.stubbe.dev/default
extra-trusted-public-keys = default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=
```

### Go

```sh
go install github.com/stubbedev/win-rdp-mcp@latest
# then install xfreerdp3 / xdotool / imagemagick / xvfb / xdpyinfo yourself
```

### npm / npx

```sh
npx -y @stubbedev/win-rdp-mcp control --target ... --user ...
```

Downloads the prebuilt binary for your platform on first run. You still need the
runtime tools installed (the npm package doesn't bundle them).

> Everything builds and tests on Linux, macOS and Windows — that is how CI checks
> it — but the **controller runs on Linux** (it needs xfreerdp/xdotool), and the
> **agent binary is for Windows**.

---

## Flags

GNU-style: every option has a `--long` form; the common ones a `-short` alias.

### Controller (`win-rdp-mcp control`)

```
-t, --target HOST[:PORT]   target RDP host (required)
-u, --user NAME            RDP username (required)
-D, --domain NAME          RDP domain (blank for a local account)
-p, --pass-file PATH       file holding the password (else $WIN_RDP_TARGET_PASS)
-W, --width N              session width  (default 1280)
-H, --height N             session height (default 800)
    --agent-exe PATH       Windows agent to push (default ./win-rdp-mcp.exe)
    --no-agent             desktop tools only; don't push the in-session agent
-d, --debug                log RDP + bootstrap detail
-a, --enable-all           enable every tool, including destructive tier 3
-3, --enable-tier3         enable the destructive tier 3 tools
-2, --disable-tier2        disable the interactive tier 2 tools
```

### Agent (`win-rdp-mcp`, runs on the target)

```
-t, --transport stdio|streamable-http|dir   default streamable-http
    --dir PATH                              exchange dir for --transport dir
-H, --host / -p, --port                     bind address (default 127.0.0.1:8090)
-k, --auth-key KEY                          also $WIN_RDP_MCP_AUTH_KEY
    --allow-insecure-remote                 non-loopback bind with no auth (danger)
    --ssl-certfile / --ssl-keyfile          enable HTTPS
    --oauth-client-id / --oauth-client-secret
-a, --enable-all  -3, --enable-tier3  -2, --disable-tier2
    --tools / -x, --exclude-tools           comma-separated
    --ip-allowlist                          comma-separated IPs/CIDRs
-d, --debug
```

Subcommands: `control`, `install`, `uninstall`, `health`.

---

## Tools

45 tools. Every result is prefixed `[task:<id>]`, which `GetTaskStatus` and
`CancelTask` take.

When running as a controller, the split below is what has a footprint on the
target and what does not. When running the agent standalone on the target, every
tool is served by the agent and the distinction disappears.

**Served locally by the controller** — driven straight over RDP with
xdotool/import, **no footprint on the target**:
`Snapshot`, `Click`, `Type`, `Move`, `Scroll`, `Shortcut`, `Wait`.

> `Snapshot` returns the screenshot on its own; once the agent is up it also
> carries the window and control list (with coordinates), so a model can aim
> clicks by element rather than by guessing pixels.

**Served by the in-session agent** — everything else, including the desktop
tools that need Win32 (window management, OCR, recording, the annotated view):

- *Desktop/Win32:* `AnnotatedSnapshot`, `FocusWindow`, `MinimizeAll`, `App`,
  `OCR`, `ScreenRecord`, `LockScreen`, `ReconnectSession`, `GetClipboard`,
  `SetClipboard`, `Notification`, `PlaySound`
- *System:* `Shell`, `ListProcesses`, `KillProcess`, `GetSystemInfo`,
  `ServiceList/Start/Stop`, `TaskList/Create/Delete`, `EventLog`, `RegRead`,
  `RegWrite`, `FileRead/Write/List/Search/Download/Upload`
- *Network:* `Ping`, `PortCheck`, `NetConnections`, `Scrape`
- *Tasks:* `GetTaskStatus`, `GetRunningTasks`, `CancelTask`

The first agent-served call after connect waits ~40s while the agent bootstraps
into the session; the locally-served tools work immediately.

---

## Security model

This hands an AI a keyboard, a mouse and — if you let it — a PowerShell prompt on
a real machine. The defaults reflect that.

### Tiers

**Tier 1 (read-only) and tier 2 (desktop interaction) are on by default; tier 3
is not.**

| Tier | What it does | Enable with |
|------|--------------|-------------|
| **1** — read-only | screenshots, OCR, recording, listings, registry reads, event log, network checks | on by default |
| **2** — desktop | Click, Type, Move, Scroll, Shortcut, FocusWindow, MinimizeAll, Scrape, ReconnectSession | on by default (`--disable-tier2` / `-2` to turn off) |
| **3** — destructive | Shell, App, PlaySound, File writes/reads, KillProcess, RegWrite, Service control, Task create/delete, SetClipboard, LockScreen | `--enable-tier3` / `-3`, or `--enable-all` / `-a` |

The tier chosen on the controller is reproduced on the pushed agent.

### Credentials

Pass the RDP password by `--pass-file` or `$WIN_RDP_TARGET_PASS`, never on the
command line (argv is world-readable via `ps`).

### Other guards

- **SSRF** — `Scrape` and `PlaySound` refuse non-public targets (loopback,
  private ranges, link-local, CGNAT) and refuse redirects.
- **PowerShell injection** — every value interpolated into a PowerShell command
  is single-quoted with its quotes doubled; `TaskCreate` takes a closed set of
  schedule types.
- **Killing by name is exact**, not fuzzy — upstream scored `notepad` against
  `notepad++` at 87, high enough to kill the wrong process.
- When the agent is run **standalone over the network**, it refuses a
  non-loopback bind with no authentication, and refuses tier 3 on such a bind
  outright.

### Two things to know

- **Defender.** Pushing an `.exe` into a session and driving input is,
  mechanically, what lateral-movement malware does. Defender may quarantine the
  agent. If the system tier never comes up, that's the first thing to check
  (run with `--debug`).
- **Authorization.** This is a remote-control pattern. Use it only on machines
  you own or are authorized to administer.

---

## Running the agent directly

If you'd rather install the agent on the target (and you have a way in — RDP
drive, SMB, a file copy), it's a standalone MCP server too:

```powershell
# local, over stdio (Claude Desktop launches it)
.\win-rdp-mcp.exe --transport stdio

# over the network, authenticated
.\win-rdp-mcp.exe --host 0.0.0.0 --port 8090 --auth-key YOUR_SECRET_KEY

# auto-start at boot as the logged-on user (needs a desktop for the GUI tools)
.\win-rdp-mcp.exe install
```

In that mode you connect your client straight to the agent and skip the
controller entirely.

---

## Development

```sh
nix develop        # Go 1.27, gopls, staticcheck, just, node — plus xfreerdp,
                   # xdotool, imagemagick, xvfb for running the controller
just               # list every recipe
```

| Recipe | What it does |
|--------|--------------|
| `just build` / `just build-windows` | build for this platform / the Windows agent |
| `just check` | the full merge gate: gofmt, vet (both platforms), tests, both builds |
| `just run` / `just run-http` | run the agent over stdio / on loopback |
| `just tools` | list the tools for a given set of flags |
| `just smoke` | drive the agent over stdio and assert `tools/list` |
| `just bundle` | pack and validate the `.mcpb` |
| `just nix-build` / `just nix-check` / `just nix-vendor-hash` | flake build / check / refresh vendorHash |
| `just release-patch` / `-minor` / `-major` | cut a release |

### Layout

| File | Holds |
|------|-------|
| `controller_linux.go` | the RDP controller: session, desktop tools, MCP routing |
| `controller_agent_linux.go` | pushing the agent into the session, the file-RPC client |
| `dirserve.go` | the agent's `dir` transport (file request/response) |
| `tools.json` / `tools.go` | tool schemas + agent registration/dispatch |
| `desktop.go` / `system.go` | the agent's Win32 and system tools |
| `platform_windows.go` / `platform_other.go` | the Win32 layer + off-Windows stubs |
| `tiers.go` `security.go` `oauth.go` `tasks.go` `config.go` | tiers, gates, OAuth, concurrency, config |

### CI

- **CI** — gofmt, vet (host + Windows cross), race tests, both builds, `.mcpb`
  validate, stdio smoke, plus the suite on a real `windows-latest` runner.
- **Flake** — recomputes `vendorHash` on any Go change and commits it, builds
  the flake, runs `nix flake check`, pushes the closure to the cache.
- **Flake update** — weekly `nix flake update`, committed only if it still builds.
- **Dependabot** — weekly Go and Actions updates, auto-merged once green.

---

## Differences from winremote-mcp

Same tool names, same tiers, same TOML config shape. What changed: it's a single
static Go binary (no Python/pywin32 on the target); it can drive the target over
**RDP alone** with nothing installed; killing a process by name is exact, not
fuzzy; the SSRF guard also blocks CGNAT and `0.0.0.0/8`; unknown config keys are
an error; env vars are `WIN_RDP_MCP_*` and the config file is `win-rdp-mcp.toml`.

## License

MIT — see [LICENSE](LICENSE).
