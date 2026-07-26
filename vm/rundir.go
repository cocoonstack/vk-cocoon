package vm

import (
	"fmt"
	"os"
)

const (
	runDirCH = "cloudhypervisor"
	runDirFC = "firecracker"
)

// COWSize returns the actual disk usage of a VM's writable overlay, or 0
// when no overlay file exists.
func COWSize(rootDir, hypervisor, vmID string) int64 {
	dir := hypervisorRunDir(hypervisor)
	for _, name := range cowFileNames(dir) {
		if fi, err := os.Stat(runPath(rootDir, dir, vmID, name)); err == nil {
			return fi.Size()
		}
	}
	return 0
}

// ConsoleSocketPath returns the CH serial-console Unix socket of a VM.
func ConsoleSocketPath(rootDir, vmID string) string {
	return runPath(rootDir, runDirCH, vmID, "console.sock")
}

// HasCloudimgOverlay reports whether the VM's on-disk state is
// cloudimg-backed (qcow2 overlay present).
func HasCloudimgOverlay(rootDir, vmID string) bool {
	_, err := os.Stat(runPath(rootDir, runDirCH, vmID, "overlay.qcow2"))
	return err == nil
}

// runPath builds a path inside a VM's runtime directory, the layout cocoon writes on disk.
func runPath(rootDir, runDir, vmID, file string) string {
	return fmt.Sprintf("%s/run/%s/%s/%s", rootDir, runDir, vmID, file)
}

// hypervisorRunDir maps the inspect "hypervisor" field to the actual
// run directory name (cocoon uses "cloudhypervisor" without a hyphen).
func hypervisorRunDir(hypervisor string) string {
	if hypervisor == BackendFirecracker {
		return runDirFC
	}
	return runDirCH
}

func cowFileNames(dir string) []string {
	if dir == runDirFC {
		return []string{"cow.raw"}
	}
	return []string{"overlay.qcow2", "cow.raw"}
}
