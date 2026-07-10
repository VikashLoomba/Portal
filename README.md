# portal

[![CI](https://github.com/VikashLoomba/Portal/actions/workflows/ci.yml/badge.svg)](https://github.com/VikashLoomba/Portal/actions/workflows/ci.yml)

Dynamic SSH port forwarding from a remote Linux dev box to your Mac — plus
transparent **clipboard paste** (images *and* text) and **notification relay**
for coding agents running on the box, over a plain `ssh` session.

Copy a screenshot or some text on your Mac, `ssh` to your dev box, and press
`Ctrl+V` inside Claude Code / opencode — the paste "just works." When the agent
finishes or needs your approval, a native macOS notification pops on your Mac.
No special `ssh` wrapper, no second daemon, no reverse tunnel: it all rides the
same SSH connection portal already maintains.

That connection is also yours to use: `portal exec` runs commands on the box
with faithful exit codes and streamed stdio — or drops you into a fully
interactive shell under a real PTY. Under the hood portal is a daemon exposing
a local control API on a Unix socket, with pluggable SSH transports and a
[public Go + TypeScript surface](#building-on-portal) for building your own
"local shell, remote brain" apps on top.

## Installation

### Recommended: download the latest release

portal ships a pre-built **Apple Silicon** (arm64) Mac binary with every
release. Download the latest one, make it executable, and run the installer:

```sh
curl -fL -o portal-darwin-arm64 \
  https://github.com/VikashLoomba/Portal/releases/latest/download/portal-darwin-arm64
chmod +x portal-darwin-arm64
./portal-darwin-arm64 install <ssh-host>
```

(Or, with the [`gh`](https://cli.github.com) CLI:
`gh release download -R VikashLoomba/Portal --pattern portal-darwin-arm64`.)

`portal install` copies the binary to `~/.local/bin/portal`, saves your dev-box
config, loads the background login agent, deploys the clipboard shims and
notification hook to the dev box, and runs a `portal doctor` self-test so you
know the paste path works before you rely on it. After it runs you can invoke
`portal` from anywhere (it prints a one-line `export PATH=...` to add if
`~/.local/bin` isn't already on your PATH). The downloaded file in the current
directory is just the bootstrap copy; you can delete it.

`<ssh-host>` may be an alias from `~/.ssh/config` or `user@hostname`. The
background daemon connects headlessly, so **key-based passwordless SSH is
required** (`ssh-copy-id <ssh-host>` if you haven't set it up). Run `install`
with no host to be prompted interactively.

### Build from source

Requires Go 1.25+. The build also cross-compiles the Linux dev-box agent
(`portald`) and embeds it into the `portal` binary.

```sh
git clone https://github.com/VikashLoomba/Portal.git
cd Portal
make build              # produces ./portal for your host architecture
./portal install <ssh-host>
```

`make portal-all` cross-compiles the Apple Silicon binary (`portal-darwin-arm64`)
— this is what CI publishes as the release artifact.

## Usage

```
portal <command>

  Setup
    install [host]  Configure the dev box and install as a login agent
                    (auto-start + self-heal); deploy the clipboard shims +
                    notification hook, then run the self-test.
    uninstall       Stop, remove the agent, restore the dev box, and tear
                    down the ssh master.
    reload          Re-apply config/plist changes.
    host [newhost]  Show the configured dev box, or switch to a new one.
    transport [system|native]
                    Show or set how portal reaches the box: system ssh
                    (ControlMaster) or native built-in ssh (resolves
                    ~/.ssh/config incl. ProxyJump). Restart to apply.

  Control
    start / stop / restart   Control the forwarding service.

  Sessions
    exec [-t|-T] [-- <cmd...>]
                    Run a command on the box over the daemon's existing
                    connection: faithful exit code, streamed stdio, audit log.
                    With a terminal and no command it opens an interactive
                    shell under a full PTY (resize and job control work);
                    -t forces a PTY for a command, -T disables it.
    ssh <host> ...  Deprecated alias for plain `ssh` (clipboard paste now works
                    over plain ssh; kept so muscle memory and scripts don't break).

  Credentials
    keychain list / keychain forget <label>   Manage remembered credentials
                                              on the Mac.
    keychain run / keychain askpass           Dev-box commands agents use to
                                              request a secret; sudo uses the
                                              same approval path transparently.

  Inspect
    status          Show box, service state, ssh master, active forwards. (default)
    doctor          Self-test the clipboard + notification path over ssh.
    ports           List the loopback dev ports currently listening on the box.
    logs [-f|N]     Show recent log lines; -f to follow, N for last N lines.
    clip-check      Diagnose Mac clipboard image detection (--upload to test).
    version         Print the portal version and build commit (also -v/--version).

  Allowlist
    allow / unallow / allowed   Manage force-forwarded ports.

  Capabilities
    features [name on|off]      Show or toggle the clip-image / clip-text /
                                notify / exec / cred gates (picked up live).
```

Run `portal help` for the full command reference, or `portal <command> --help`
for a command's flags.

## Remote commands and shells

`portal exec` rides the same authenticated connection the daemon already
maintains — no extra ssh handshake, no password prompt:

```sh
portal exec -- ls -la              # exit code, stdout, stderr all faithful
portal exec -- make test           # stream a long build; Ctrl+C propagates
portal exec                        # interactive shell on the box, full PTY
portal exec -t -- htop             # force a PTY for a specific command
```

Interactive sessions get a real pseudo-terminal: raw mode, window-resize
propagation (SIGWINCH), job control, and clean terminal restore even when
killed by a signal. Every exec session is recorded (argv, uid, exit code,
duration, session id) in the append-only audit log, and the whole feature can
be switched off with `portal features exec off`.

## Transports

portal reaches the dev box through a pluggable transport, selectable with
`portal transport`:

- **system** (default) — drives your system `ssh` via a `ControlMaster`
  multiplexed connection. Uses your existing ssh setup exactly as-is.
- **native** — a built-in pure-Go SSH client (`golang.org/x/crypto/ssh`). It
  resolves hosts through `~/.ssh/config` (via `ssh -G`), dials `ProxyJump` /
  `ProxyCommand` chains itself, and enforces strict `known_hosts` checking.
  No `ssh` processes are spawned for the connection.

Both implement the same transport contract (as does a third, `localexec`, used
for same-machine development), verified by a shared conformance suite.

## How clipboard paste works

The coding agent already owns `Ctrl+V`; on paste it shells out to `xclip` /
`wl-paste` to read the clipboard. portal installs tiny shims for those tools
earlier on the dev box's `PATH`. When the agent reads the clipboard, the shim
relays the request up the **existing** SSH/`portald` connection to your Mac,
which reads its *real* clipboard and sends the bytes back — so plain
`ssh <host>` then `claude` (or `opencode`) is all you need.

- **Images** are coerced to PNG, pushed over the SSH connection to
  `~/.cache/portal/clip/` on the box (content-addressed, mode `0600`), and the
  agent ingests them as `[Image #1]`.
- **Text** is served the same way.
- If the Mac clipboard has nothing servable, the shim cleanly falls through to
  the real `xclip`/`wl-paste`, so non-agent clipboard use is unaffected.

Run `portal doctor` any time to verify the path end to end — it checks that the
shims win the `PATH` race on the box, that their version matches, that the agent
is connected, and runs a live round-trip smoke test.

> **Heads-up — keep your terminal's OSC 52 clipboard-*write* disabled.** portal
> no longer proxies the session, so it can't strip remote OSC 52 writes. With
> clipboard *read* now available to the box, a hostile remote could otherwise
> write your Mac clipboard via OSC 52 and read it straight back. Most terminals
> ship with OSC 52 write off by default; leave it that way.

## Notifications

portal installs a Claude Code hook on the dev box. When Claude stops, needs a
tool approval, or otherwise notifies, the event is relayed up the same
connection and raised as a native macOS notification. Events that arrive through
the structured hook are trusted; a generic `portald notify --title … --body …`
is rendered with an `[unverified]` prefix.

## Credential sharing (`portal keychain`)

When an agent needs a login secret, it can wrap the command on the **dev box**
so the secret goes directly into the child process instead of through the
conversation. For example:

```sh
portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" …'
```

The single quotes are important: they make the child shell expand `$PW`; the
caller's shell must not expand it. `--stdin` is also available when the child
expects the secret on standard input.

For `sudo`, portal's dev-box `sudo` shim and `SUDO_ASKPASS` helper take the same
path transparently when the agent has no controlling terminal. Any session in
which a human could still be prompted is a direct passthrough to the real sudo.

> **Heads-up — transparent `sudo` is deliberately fail-safe around shared
> terminals.** It fires only for an agent with **no controlling terminal**. In
> a shared interactive SSH session the agent shares the human's tty, so portal
> does not auto-intercept; use `portal keychain run …` there, or approve sudo
> yourself. This tradeoff prevents portal from hijacking a human password
> prompt, including when sudo's stdin has been redirected.

Each request opens a native secure-input dialog on the Mac showing which
process requested it, which box it came from, and how the secret will be
delivered. Choosing **Allow & Remember** stores the secret in the macOS
Keychain; later requests for that label are click-to-approve confirmations
rather than another password entry. On the Mac, `portal keychain list` shows
remembered labels; `portal keychain forget <label>` removes one. Run
`portal features cred off` for the immediate kill switch that disables
credential prompts and delivery.

> **Heads-up — credential sharing protects the agent transcript, not a hostile
> same-UID process on the box.** The guarantee is that the secret never enters
> the agent's context window or transcript, process argv, portal logs, or the
> box's disk; it travels in memory from the Mac Keychain/dialog to the consumer
> process. It is **not** a defense against an actively malicious process running
> as the same box user, which can read `/proc/<pid>/environ` or ptrace another
> process. The consent dialog and audit log are the control points.

## Capability gates

Clipboard-read, notifications, exec, and credential requests are **on by
default** (matching the install experience you'd expect) but are individually
gated on the Mac. Toggle them with `portal features <name> on|off` (or edit the
file under `~/.config/portal/` directly); the running daemon picks changes up
with no restart:

| Gate | File | Gates |
|---|---|---|
| `clip-image` | `feature.clip-image` | serving the Mac clipboard **image** to the dev box |
| `clip-text`  | `feature.clip-text`  | serving the Mac clipboard **text** to the dev box |
| `notify`     | `feature.notify`     | raising notifications relayed from the dev box |
| `exec`       | `feature.exec`       | running commands on the box via `portal exec` / the control API |
| `cred` | `feature.cred` | approving credential requests from the dev box (`portal keychain` / sudo askpass) |

Clipboard **text** marked secret by a password manager (the macOS
`org.nspasteboard.ConcealedType` hint) is never served, regardless of the
toggle. Every served read, credential outcome, and notification is recorded to
an append-only audit log under the portal config directory.

There is no bearer token (cc-clip needs one because it serves clipboard over a
loopback HTTP server any local process can reach). portal's transport is the
authenticated SSH `ControlMaster` pipe plus an owner-only (`0600`) Unix socket,
which together are the network and local trust boundary the token stood in for.

## Building on portal

portal doubles as a platform for "local shell, remote brain" desktop apps: the
daemon exposes everything it can do over a local control API, and the pieces it
is built from are public packages.

- **Control API** — a Unix socket at `~/.config/portal/api.sock` (owner-only,
  `0600`) serving versioned HTTP + WebSocket endpoints: status, ports, feature
  gates, and `/v1/exec` (streamed exec with optional PTY). Schemas live in
  [`internal/localapi/openapi.yaml`](internal/localapi/openapi.yaml).
- **Go packages** — the public surface under [`pkg/`](pkg/): `protocol` (CBOR
  wire codec), `transport` (+ `sshctl`, `sshnative`, `localexec`, a shared
  conformance suite, and `ptyx` for PTY allocation), `run` (daemon host),
  `client` (control-API client), `agent`/`agentclient` (dev-box side), and
  more. `pkg/` never imports `internal/` — enforced by a test.
- **Wire spec** — the Mac↔box protocol is specified in
  [`docs/wire.cddl`](docs/wire.cddl) with golden vectors under
  [`docs/vectors/`](docs/vectors/), verified from both Go and TypeScript, so a
  client in any language can prove itself conformant.
- **TypeScript client** — [`clients/ts`](clients/ts) is a zero-runtime-dependency
  client for the control API, and
  [`examples/shell-electron`](examples/shell-electron) is a minimal Electron
  app driving a remote PTY session through it.

## Requirements

- An **Apple Silicon Mac** (arm64) for the client.
- A **Linux dev box** reachable over passwordless (key-based) SSH, with a
  POSIX shell and `xclip`/`wl-paste` resolvable through portal's shims.
- A supported coding agent for paste: **Claude Code** or **opencode**.
  **Codex is not supported** — it reads the X11/Wayland clipboard in-process
  (via the `arboard` crate), which a `PATH` shim cannot intercept.
