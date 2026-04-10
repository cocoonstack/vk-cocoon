package main

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeCapacity returns the advertised capacity of the virtual
// cocoon node. The numbers are intentionally generous — the cocoon
// runtime caps each VM at its own limits, so this acts as a ceiling
// that keeps the K8s scheduler from refusing to place pods on the
// virtual node. Overlays can override VK_NODE_CPU / VK_NODE_MEM /
// VK_NODE_PODS to match the actual host budget.
func NodeCapacity() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(envOrDefault("VK_NODE_CPU", "128")),
		corev1.ResourceMemory: resource.MustParse(envOrDefault("VK_NODE_MEM", "512Gi")),
		corev1.ResourcePods:   resource.MustParse(envOrDefault("VK_NODE_PODS", "256")),
	}
}
