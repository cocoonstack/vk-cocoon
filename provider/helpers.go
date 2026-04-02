package provider

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeCapacity returns the resource capacity for the virtual node.
// Reads real CPU/MEM from host /proc (like kubelet cAdvisor).
func NodeCapacity() corev1.ResourceList {
	cpuCount := readHostCPUCount()
	if cpuCount <= 0 {
		cpuCount = 256 // fallback
	}
	memBytes := readHostMemoryBytes()
	if memBytes == 0 {
		memBytes = 1536 * 1024 * 1024 * 1024 // fallback 1536Gi
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(int64(cpuCount), resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(int64(memBytes), resource.BinarySI), //nolint:gosec // memBytes fits in int64
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(3500*1024*1024*1024, resource.BinarySI),
		corev1.ResourcePods:             *resource.NewQuantity(241, resource.DecimalSI),
	}
}

// skipSSH returns true when the VM cannot be reached via SSH.
// This is the case for Windows guests (which use RDP) or when
// no IP has been assigned yet.
func (vm *CocoonVM) skipSSH() bool {
	return vm.os == osWindows || vm.ip == ""
}

// podKey builds the canonical map key for a pod: "namespace/name".
func podKey(ns, name string) string { return ns + "/" + name }

// ann reads an annotation from a pod, returning def when absent or empty.
func ann(pod *corev1.Pod, key, def string) string {
	if v, ok := pod.Annotations[key]; ok && v != "" {
		return v
	}
	return def
}

// parseImageRef splits an image reference into (registryURL, snapshotName).
// Supports:
//
//	"https://registry.example.com/ubuntu-dev-base" → ("https://registry.example.com", "ubuntu-dev-base")
//	"http://127.0.0.1:4300/ubuntu-dev-base"        → ("http://127.0.0.1:4300", "ubuntu-dev-base")
//	"ubuntu-dev-base"                               → ("", "ubuntu-dev-base")
func parseImageRef(image string) (registryURL, snapshotName string) {
	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		idx := strings.LastIndex(image, "/")
		if idx > 8 { // after "https://"
			return image[:idx], image[idx+1:]
		}
	}
	return "", image
}

// shellQuoteJoin joins command args into a single shell-safe string.
// For SSH, the remote command is passed as a single string to the remote shell.
func shellQuoteJoin(args []string) string {
	if len(args) == 0 {
		return ""
	}
	// If the command is "sh -c <script>", pass the script directly.
	if len(args) >= 3 && args[0] == "sh" && args[1] == "-c" {
		return strings.Join(args[2:], " ")
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t'\"\\$(){}|&;<>!") {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

func parseVMID(out string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vm-") {
			return line
		}
	}
	return ""
}

// formatResourceMemory converts a Kubernetes memory quantity to a cocoon
// CLI-friendly string like "8G" or "512M".
func formatResourceMemory(q *resource.Quantity) string {
	if q == nil || q.IsZero() {
		return ""
	}
	mb := q.Value() / (1024 * 1024)
	if mb >= 1024 {
		return fmt.Sprintf("%dG", mb/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

// formatResourceCPU converts a Kubernetes CPU quantity to a string like "2".
func formatResourceCPU(q *resource.Quantity) string {
	if q == nil || q.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d", q.Value())
}

// podResourceLimits extracts CPU and memory limits from a pod spec,
// returning cocoon CLI-friendly strings with sensible defaults.
func podResourceLimits(pod *corev1.Pod) (cpu, mem string) {
	cpu = "2"
	mem = defaultMemoryForOS(ann(pod, AnnOS, defaultOSType))
	if c := pod.Spec.Containers; len(c) > 0 {
		if s := formatResourceCPU(c[0].Resources.Limits.Cpu()); s != "" {
			cpu = s
		}
		if s := formatResourceMemory(c[0].Resources.Limits.Memory()); s != "" {
			mem = s
		}
	}
	return cpu, mem
}

func defaultMemoryForOS(osType string) string {
	if osType == osWindows {
		return "4G"
	}
	return "8G"
}

func parseCPUString(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 2
	}
	return n
}

func parseMemoryStringMB(raw string) int {
	raw = strings.TrimSpace(strings.ToUpper(raw))
	switch {
	case raw == "":
		return 8192
	case strings.HasSuffix(raw, "GI"), strings.HasSuffix(raw, "G"):
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "I"), "G")
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n * 1024
		}
	case strings.HasSuffix(raw, "MI"), strings.HasSuffix(raw, "M"):
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "I"), "M")
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return 8192
}
