package guest

import (
	"context"
	"fmt"
	"io"
)

// RDPExecutor is a Windows guest stand-in that returns "use RDP" help text.
type RDPExecutor struct{}

// Run writes a "use RDP" message to stdout.
func (RDPExecutor) Run(_ context.Context, host string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
	if stdout != nil {
		_, _ = fmt.Fprintf(stdout,
			"vk-cocoon: kubectl exec is not supported on Windows guests. "+
				"Connect via RDP to %s.\n", host)
	}
	return nil
}
