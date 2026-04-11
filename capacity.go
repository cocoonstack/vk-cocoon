package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

// NodeCapacity returns the advertised capacity of the virtual
// cocoon node. The numbers are intentionally generous — the cocoon
// runtime caps each VM at its own limits, so this acts as a ceiling
// that keeps the K8s scheduler from refusing to place pods on the
// virtual node. Overlays can override VK_NODE_CPU / VK_NODE_MEM /
// VK_NODE_PODS to match the actual host budget. Malformed overrides
// surface as an error rather than a panic from MustParse so the
// caller can log context and exit cleanly.
func NodeCapacity() (corev1.ResourceList, error) {
	cpu, err := parseQuantityEnv("VK_NODE_CPU", "128")
	if err != nil {
		return nil, err
	}
	mem, err := parseQuantityEnv("VK_NODE_MEM", "512Gi")
	if err != nil {
		return nil, err
	}
	pods, err := parseQuantityEnv("VK_NODE_PODS", "256")
	if err != nil {
		return nil, err
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: mem,
		corev1.ResourcePods:   pods,
	}, nil
}

func parseQuantityEnv(key, fallback string) (resource.Quantity, error) {
	raw := commonk8s.EnvOrDefault(key, fallback)
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parse %s=%q: %w", key, raw, err)
	}
	return q, nil
}
