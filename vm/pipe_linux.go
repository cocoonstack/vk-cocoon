//go:build linux

package vm

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	transferPipeBytes = 8 << 20
	pipeMaxSizePath   = "/proc/sys/fs/pipe-max-size"
)

func widenPipe(p any) error {
	f, ok := p.(*os.File)
	if !ok {
		return nil
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, transferPipeBytes); err == nil {
		return nil
	} else if !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("set pipe size: %w", err)
	}
	limit, err := readPipeMaxSize()
	if err != nil {
		return err
	}
	if _, err = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, min(limit, transferPipeBytes)); err != nil {
		return fmt.Errorf("set pipe fallback size: %w", err)
	}
	return nil
}

func readPipeMaxSize() (int, error) {
	raw, err := os.ReadFile(pipeMaxSizePath)
	if err != nil {
		return 0, fmt.Errorf("read pipe max size: %w", err)
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse pipe max size %q: %w", strings.TrimSpace(string(raw)), err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("pipe max size must be positive, got %d", size)
	}
	return size, nil
}
