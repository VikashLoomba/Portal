# portal

Dynamic SSH port forwarding from a remote Linux dev box to your Mac — plus
clipboard-image paste over SSH (press `Ctrl+V` in a `portal ssh` session to
upload a copied screenshot and insert its remote path).

## Installation

### Recommended: download the latest release

Pre-built Mac binaries are published on the
[**Releases page**](https://gitlab.i.extrahop.com/vikashl/devportal/-/releases).
Download the binary that matches your Mac, then install it.

Pick the right build for your Mac (run `uname -m` if unsure):

| Mac | `uname -m` | Asset |
|-----|------------|-------|
| Apple Silicon (M1/M2/M3/M4) | `arm64` | `portal-darwin-arm64` |
| Intel | `x86_64` | `portal-darwin-amd64` |

Then, from your Downloads folder:

```sh
# Rename to `portal`, make it executable, and put it on your PATH.
mv portal-darwin-arm64 portal        # or portal-darwin-amd64 on an Intel Mac
chmod +x portal

# macOS quarantines binaries downloaded from a browser; clear it so Gatekeeper
# doesn't block the first run (otherwise: right-click → Open, once).
xattr -d com.apple.quarantine portal 2>/dev/null || true

# Move onto your PATH (or keep it wherever you like and call it by path).
sudo mv portal /usr/local/bin/portal

# Configure your dev box and install the background login agent.
portal install <ssh-host>
```

Prefer the command line? Grab the latest tag's asset directly (set `ARCH` to
`arm64` or `amd64`):

```sh
ARCH=arm64
VERSION=$(git ls-remote --tags --sort=-v:refname \
  https://gitlab.i.extrahop.com/vikashl/devportal.git \
  | sed -n 's#.*refs/tags/\(v[0-9.]*\)$#\1#p' | head -1)

curl -fL -o portal \
  "https://gitlab.i.extrahop.com/vikashl/devportal/-/jobs/artifacts/${VERSION}/raw/portal-darwin-${ARCH}?job=release-build"
chmod +x portal
xattr -d com.apple.quarantine portal 2>/dev/null || true
sudo mv portal /usr/local/bin/portal
portal install <ssh-host>
```

`<ssh-host>` may be an alias from `~/.ssh/config` or `user@hostname`. The
background daemon connects headlessly, so **key-based passwordless SSH is
required** (`ssh-copy-id <ssh-host>` if you haven't set it up). Run `portal
install` with no host to be prompted interactively.

### Build from source

Requires Go 1.25+. The build also cross-compiles the Linux dev-box agent
(`portald`) and embeds it into the `portal` binary.

```sh
git clone https://gitlab.i.extrahop.com/vikashl/devportal.git
cd devportal
make build              # produces ./portal for your host architecture
./portal install <ssh-host>
```

`make portal-all` builds both Mac architectures (`portal-darwin-amd64` and
`portal-darwin-arm64`) — this is what CI publishes as release artifacts.

## Usage

```
portal <command>

  Setup
    install [host]  Configure the dev box and install as a login agent
                    (auto-start + self-heal), then start it.
    uninstall       Stop, remove the agent, and tear down the ssh master.
    reload          Re-apply config/plist changes.
    host [newhost]  Show the configured dev box, or switch to a new one.

  Control
    start / stop / restart   Control the forwarding service.

  Sessions
    ssh <host> ...  SSH to a host with clipboard-image paste: press Ctrl+V to
                    upload a copied screenshot and insert its remote path.
                    Forwards all extra args to ssh (drop-in replacement).

  Inspect
    status          Show box, service state, ssh master, active forwards. (default)
    ports           List the loopback dev ports currently listening on the box.
    logs [-f|N]     Show recent log lines; -f to follow, N for last N lines.

  Allowlist
    allow / unallow / allowed   Manage force-forwarded ports.
```

Run `portal help` for the full command reference.

### Clipboard-image paste

`portal ssh <host>` opens an interactive SSH session proxied through a PTY so
portal can intercept `Ctrl+V`. When you press `Ctrl+V` and your Mac clipboard
holds an image, portal uploads it to `~/.cache/portal/clip/` on the remote (over
the **same** SSH connection) and types the resulting path at your cursor — so
screenshots paste straight into a coding agent running on the dev box. If the
clipboard has no image, `Ctrl+V` passes through unchanged.

Run `portal clip-check` to diagnose clipboard image detection (add `--upload` to
test the full upload path to your configured dev box).

## Requirements

- A **Mac** (Apple Silicon or Intel) for the client.
- A **Linux dev box** reachable over passwordless (key-based) SSH.
