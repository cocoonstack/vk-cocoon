// Package guest defines the Executor interface for bridging operations
// into cocoon guest VMs. Subpackages provide concrete implementations:
// ssh (Linux), rdp (Windows help-text), sac (Windows SAC serial console).
package guest

import (
	"context"
	"io"
	"strings"
)

// Executor runs a command inside a guest VM. The target parameter is
// executor-specific: an IP address for SSH/RDP, a Unix socket path for
// SAC. The caller decides what to run; the executor decides how.
type Executor interface {
	Run(ctx context.Context, target string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// JoinShell joins argv into a POSIX-sh-safe command string for ssh.Session.Run.
func JoinShell(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = PosixQuote(a)
	}
	return strings.Join(parts, " ")
}

// PosixQuote wraps s in POSIX single quotes, passing simple strings through unchanged.
func PosixQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsShellQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsShellQuote(s string) bool {
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			continue
		case c == '_', c == '-', c == '.', c == '/', c == ':', c == '=', c == '@', c == '+', c == ',':
			continue
		default:
			return true
		}
	}
	return false
}
