// Package guest defines the console-session contract for bridging into
// cocoon guest VMs; the sac subpackage implements it for the Windows SAC
// serial console. Command exec goes through cocoon-agent, not this package.
package guest

import (
	"context"
	"io"
)

// Session holds a persistent connection to a guest console, preserving state between calls (e.g. SAC serial port state).
type Session interface {
	Run(ctx context.Context, cmd []string, stdout io.Writer) error
	Close() error
}

// Dialer opens a persistent Session to a guest.
type Dialer interface {
	Dial(ctx context.Context, target string) (Session, error)
}
