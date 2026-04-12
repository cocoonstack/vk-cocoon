package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

// NodeCapacity returns the advertised capacity of the virtual node.
// Values are generous defaults; override via VK_NODE_CPU / VK_NODE_MEM / VK_NODE_PODS.
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
