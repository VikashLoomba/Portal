package localexec_test

import (
	"testing"

	"github.com/VikashLoomba/Portal/internal/transport"
	"github.com/VikashLoomba/Portal/internal/transport/conformance"
	"github.com/VikashLoomba/Portal/internal/transport/localexec"
)

// The shared suite proves localexec under the shell-join model. The
// PortForwarder section is skipped because Local does not implement it.
func TestConformance(t *testing.T) {
	conformance.Run(t, "localexec", func(t *testing.T) transport.Transport {
		return localexec.New()
	})
}
