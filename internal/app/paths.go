// Package app holds the composition root: identity constants, derived paths,
// tunables, and the App dependency container that command files consume.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tool is the single source of truth for the tool's identity. Change this one
// line to rebrand — every path/label/log derives from it.
const Tool = "portal"

// Tunables are deliberately Go constants, NOT environment variables: the
// launchd daemon runs with a clean environment (only PATH + HOME), so a shell
// `export` would never reach the running service. In-script constants are the
// honest single source of truth — same rationale as the bash original.
const (
	// Interval is the period between reconcile passes.
	Interval = 10 * time.Second
	// LsofPath is the absolute path to lsof; constant because launchd PATH
	// search is unreliable across macOS variants.
	LsofPath = "/usr/sbin/lsof"
	// PlistPATH is the PATH the launchd-spawned daemon sees. Includes
	// /usr/sbin (lsof), /opt/homebrew/bin (Apple Silicon brew),
	// /usr/local/bin (Intel brew).
	PlistPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// DenyPorts are remote system services we never forward.
//   22 ssh, 25 smtp, 53 dns, 631 cups, 139/445 smb/netbios.
var DenyPorts = []int{22, 25, 53, 631, 139, 445}

// SkipLocal lists extra LOCAL ports to never bind here (in case something
// local needs them). Empty by default.
var SkipLocal = []int{}

// SSHOpts are passed to every ssh invocation (master + multiplexed calls).
var SSHOpts = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=12",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
}

// Paths are the derived file/path locations for this run.
type Paths struct {
	Home      string
	UID       int
	ConfigDir string // honors PORTAL_CONFIG_DIR (test seam)
	HostFile  string
	AllowFile string
	Label     string
	BinDir    string
	BinPath   string
	Plist     string
	Log       string
	Sock      string // honors PORTAL_SOCK (test seam)
	Domain    string // gui/<uid>
}

// DerivePaths computes Paths from the user's HOME and uid. PORTAL_CONFIG_DIR
// and PORTAL_SOCK are honored as test seams: setting them lets a developer
// run an isolated Go instance without disturbing the installed service.
// Normal use never sets them.
func DerivePaths(home string, uid int) Paths {
	cfg := os.Getenv("PORTAL_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(home, ".config", Tool)
	}
	sock := os.Getenv("PORTAL_SOCK")
	if sock == "" {
		sock = filepath.Join(home, ".ssh", "cm-"+Tool+".sock")
	}
	binDir := filepath.Join(home, ".local", "bin")
	label := "local." + Tool + ".autoforward"
	return Paths{
		Home:      home,
		UID:       uid,
		ConfigDir: cfg,
		HostFile:  filepath.Join(cfg, "host"),
		AllowFile: filepath.Join(cfg, "allow"),
		Label:     label,
		BinDir:    binDir,
		BinPath:   filepath.Join(binDir, Tool),
		Plist:     filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		Log:       filepath.Join(home, "Library", "Logs", Tool+".log"),
		Sock:      sock,
		Domain:    fmt.Sprintf("gui/%d", uid),
	}
}

// ResolveSelf returns the absolute path of the running binary, following any
// symlinks. Used by `install` to copy the running binary to BinPath.
func ResolveSelf() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	abs, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil
	}
	return abs, nil
}
