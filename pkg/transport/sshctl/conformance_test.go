package sshctl_test

import (
	"os"
	"testing"

	"github.com/VikashLoomba/Portal/internal/proc"
	"github.com/VikashLoomba/Portal/pkg/run"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/conformance"
	"github.com/VikashLoomba/Portal/pkg/transport/sshctl"
)

// TestConformance runs the shared transport conformance suite against a real
// system-ssh master. It requires a reachable host and so is gated on
// PORTAL_TEST_SSH_HOST — when unset it skips (naming the variable) rather than
// failing on a machine with no dev box. Because Describe().Impl == "system-ssh",
// the PortForwarder section runs its lifecycle-only path (Forward/ListForwards/
// Cancel/dial-refused) and skips the cross-machine loopback echo round-trip.
func TestConformance(t *testing.T) {
	host := os.Getenv("PORTAL_TEST_SSH_HOST")
	if host == "" {
		t.Skip("set PORTAL_TEST_SSH_HOST to run the sshctl conformance suite against a live host")
	}
	runner := run.OSRunner{}
	conformance.Run(t, "sshctl", func(t *testing.T) transport.Transport {
		sock := t.TempDir() + "/cm.sock"
		s := sshctl.New(sock, host, nil, runner)
		s.Forwards = proc.New("lsof", runner)
		return s
	})
}
