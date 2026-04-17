package cocoon

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProcNetDev parses /proc/<pid>/net/dev for a specific interface.
// The hypervisor process runs inside the VM's netns, so its procfs
// view contains the namespaced network devices including the TAP.
func readProcNetDev(pid int, iface string) (rxBytes, txBytes uint64) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return 0, 0
	}
	defer f.Close() //nolint:errcheck

	prefix := iface + ":"
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line[len(prefix):])
		if len(fields) < 9 {
			return 0, 0
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		return rx, tx
	}
	return 0, 0
}
