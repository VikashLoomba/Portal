package localexec_test

import (
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/conformance"
	"github.com/VikashLoomba/Portal/pkg/transport/localexec"
)

// The shared suite proves localexec under the shell-join model. The
// PortForwarder section is skipped because Local does not implement it.
func TestConformance(t *testing.T) {
	conformance.Run(t, "localexec", func(t *testing.T) transport.Transport {
		return localexec.New()
	})
}
