// Package discover asks the remote dev box which loopback dev ports are
// listening. It composes a Transport (the multiplexed SSH ControlMaster) and
// runs an embedded ss/awk script with deny+allow lists passed as positional
// args separated by a "--" sentinel.
package discover

import (
	"bufio"
	"context"
	_ "embed"
	"sort"
	"strconv"
	"strings"

	"github.com/vikashl/portal/internal/sshctl"
)

//go:embed remote.sh
var remoteScript string

// RemoteDiscoverer returns the desired forward-set: which loopback dev ports
// are listening remotely after deny/ephemeral exclusions and allowlist
// overrides.
type RemoteDiscoverer interface {
	DesiredPorts(ctx context.Context, deny, allow []int) ([]int, error)
}

type SS struct {
	T sshctl.Transport
}

func New(t sshctl.Transport) *SS { return &SS{T: t} }

// DesiredPorts: argv = ["bash", "-s", deny..., "--", allow...]; remote.sh on
// stdin. err != nil ONLY if the SSH call itself failed — the engine treats
// that as "keep current forwards" rather than canceling everything on a
// transient blip.
func (s *SS) DesiredPorts(ctx context.Context, deny, allow []int) ([]int, error) {
	argv := []string{"bash", "-s"}
	for _, p := range deny {
		argv = append(argv, strconv.Itoa(p))
	}
	argv = append(argv, "--")
	for _, p := range allow {
		argv = append(argv, strconv.Itoa(p))
	}
	stdout, err := s.T.Exec(ctx, remoteScript, argv...)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{})
	out := make([]int, 0, 16)
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil || n <= 0 {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

var _ RemoteDiscoverer = (*SS)(nil)
