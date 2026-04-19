// Package rdp implements a guest.Executor stub for Windows guests that
// returns "use RDP" help text.
package rdp

import (
	"context"
	"fmt"
	"io"

	"github.com/cocoonstack/vk-cocoon/guest"
)

// compile-time interface check.
var _ guest.Executor = Executor{}

// Executor is a Windows guest stand-in that returns "use RDP" help text.
type Executor struct{}

// Run writes a "use RDP" message to stdout.
func (Executor) Run(_ context.Context, host string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
	if stdout != nil {
		_, _ = fmt.Fprintf(stdout,
			"vk-cocoon: kubectl exec is not supported on Windows guests. "+
				"Connect via RDP to %s.\n", host)
	}
	return nil
}
