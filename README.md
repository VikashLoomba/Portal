# portal

Dynamic SSH port forwarding from a remote Linux dev box to your Mac — plus
transparent **clipboard paste** (images *and* text) and **notification relay**
for coding agents running on the box, over a plain `ssh` session.

Copy a screenshot or some text on your Mac, `ssh` to your dev box, and press
`Ctrl+V` inside Claude Code / opencode — the paste "just works." When the agent
finishes or needs your approval, a native macOS notification pops on your Mac.
No special `ssh` wrapper, no second daemon, no reverse tunnel: it all rides the
same SSH connection portal already maintains.

## Installation

### Recommended: download the latest release

portal ships a pre-built **Apple Silicon** (arm64) Mac binary with every
release. Download the latest one with the
[`glab`](https://gitlab.com/gitlab-org/cli) CLI (it uses your existing GitLab
login), make it executable, and run the installer:

```sh
glab release download \
  -R https://gitlab.i.extrahop.com/vikashl/devportal \
  --asset-name=portal-darwin-arm64
chmod +x portal-darwin-arm64
./portal-darwin-arm64 install <ssh-host>
```

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
git clone https://gitlab.i.extrahop.com/vikashl/devportal.git
cd devportal
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

  Control
    start / stop / restart   Control the forwarding service.

  Inspect
    status          Show box, service state, ssh master, active forwards. (default)
    doctor          Self-test the clipboard + notification path over ssh.
    ports           List the loopback dev ports currently listening on the box.
    logs [-f|N]     Show recent log lines; -f to follow, N for last N lines.
    clip-check      Diagnose Mac clipboard image detection (--upload to test).
    version         Print the portal version and build commit (also -v/--version).

  Allowlist
    allow / unallow / allowed   Manage force-forwarded ports.

  Sessions
    ssh <host> ...  Deprecated alias for plain `ssh` (clipboard paste now works
                    over plain ssh; kept so muscle memory and scripts don't break).
```

Run `portal help` for the full command reference.

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

## Capability gates

Clipboard-read and notifications are **on by default** (matching the install
experience you'd expect) but are individually gated on the Mac. Each is a file
under `~/.config/portal/`; write `off` to disable, `rm` (or `on`) to re-enable —
the running daemon picks it up with no restart:

| File | Gates |
|---|---|
| `feature.clip-image` | serving the Mac clipboard **image** to the dev box |
| `feature.clip-text`  | serving the Mac clipboard **text** to the dev box |
| `feature.notify`     | raising notifications relayed from the dev box |

Clipboard **text** marked secret by a password manager (the macOS
`org.nspasteboard.ConcealedType` hint) is never served, regardless of the
toggle. Every served read and notification is recorded to an append-only audit
log under the portal config directory.

There is no bearer token (cc-clip needs one because it serves clipboard over a
loopback HTTP server any local process can reach). portal's transport is the
authenticated SSH `ControlMaster` pipe plus an owner-only (`0600`) Unix socket,
which together are the network and local trust boundary the token stood in for.

## Requirements

- An **Apple Silicon Mac** (arm64) for the client.
- A **Linux dev box** reachable over passwordless (key-based) SSH, with a
  POSIX shell and `xclip`/`wl-paste` resolvable through portal's shims.
- A supported coding agent for paste: **Claude Code** or **opencode**.
  **Codex is not supported** — it reads the X11/Wayland clipboard in-process
  (via the `arboard` crate), which a `PATH` shim cannot intercept.
