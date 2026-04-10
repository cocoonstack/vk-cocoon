package guest

import (
	"context"
	"fmt"
	"io"
)

// RDPExecutor is a Windows guest stand-in. Windows VMs do not have
// a kubectl-style log/exec channel, so this executor returns
// helpful messages instead of attempting to run anything.
type RDPExecutor struct{}

// Run on Windows always returns a "use RDP" message; the caller is
// expected to surface it to the user as the kubectl exec output.
func (RDPExecutor) Run(_ context.Context, host string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
	if stdout != nil {
		_, _ = fmt.Fprintf(stdout,
			"vk-cocoon: kubectl exec is not supported on Windows guests. "+
				"Connect via RDP to %s.\n", host)
	}
	return nil
}
