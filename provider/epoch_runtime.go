package provider

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

func (p *EpochPuller) cocoonExec(ctx context.Context, args ...string) (string, error) {
	var sudoArgs []string
	root := strings.TrimSpace(os.Getenv("COCOON_ROOT_DIR"))
	if root == "" {
		root = strings.TrimSpace(p.rootDir)
	}
	if root != "" {
		sudoArgs = append(sudoArgs, "env", "COCOON_ROOT_DIR="+root)
	}
	if windowsCH := strings.TrimSpace(os.Getenv("COCOON_WINDOWS_CH_BINARY")); windowsCH != "" {
		if len(sudoArgs) == 0 {
			sudoArgs = append(sudoArgs, "env")
		}
		sudoArgs = append(sudoArgs, "COCOON_WINDOWS_CH_BINARY="+windowsCH)
	}
	sudoArgs = append(sudoArgs, p.cocoonBin)
	sudoArgs = append(sudoArgs, args...)
	cmd := exec.CommandContext(ctx, "sudo", sudoArgs...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	return string(out), err
}
