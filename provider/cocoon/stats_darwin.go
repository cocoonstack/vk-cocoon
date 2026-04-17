package cocoon

// readProcNetDev is a no-op on darwin (no /proc/<pid>/net/dev).
func readProcNetDev(_ int, _ string) (rxBytes, txBytes uint64) {
	return 0, 0
}
