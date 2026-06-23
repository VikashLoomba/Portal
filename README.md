# portal

Dynamic SSH port forwarding from a remote Linux dev box to your Mac — plus
clipboard-image paste over SSH (press `Ctrl+V` in a `portal ssh` session to
upload a copied screenshot and insert its remote path).

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
config, and loads the background login agent — so after it runs you can invoke
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

- An **Apple Silicon Mac** (arm64) for the client.
- A **Linux dev box** reachable over passwordless (key-based) SSH.
