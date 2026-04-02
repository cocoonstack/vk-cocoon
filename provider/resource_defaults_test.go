package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodResourceLimitsLinuxDefaults(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnOS: "linux"},
		},
	}

	cpu, mem := podResourceLimits(pod)
	if cpu != "2" || mem != "8G" {
		t.Fatalf("linux defaults = (%s, %s), want (2, 8G)", cpu, mem)
	}
}

func TestPodResourceLimitsWindowsDefaults(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnOS: "windows"},
		},
	}

	cpu, mem := podResourceLimits(pod)
	if cpu != "2" || mem != "4G" {
		t.Fatalf("windows defaults = (%s, %s), want (2, 4G)", cpu, mem)
	}
}

func TestFallbackCreatedVMUsesPlanResources(t *testing.T) {
	vm := fallbackCreatedVM(createPlan{
		vmName: "windows-vm",
		cpu:    "2",
		mem:    "4G",
	})

	if vm.cpu != 2 || vm.memoryMB != 4096 {
		t.Fatalf("fallbackCreatedVM = (%d, %dMB), want (2, 4096MB)", vm.cpu, vm.memoryMB)
	}
}
