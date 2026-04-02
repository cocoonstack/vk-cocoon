package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolvePodSpec(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnImage:        "https://registry.example.com/base-snapshot",
				AnnStorage:      "40G",
				AnnNICs:         "2",
				AnnDNS:          "1.1.1.1,8.8.8.8",
				AnnRootPassword: "secret",
				AnnOS:           "windows",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "ignored-by-annotation"}},
		},
	}

	got := resolvePodSpec(pod)
	if got.imageRaw != "https://registry.example.com/base-snapshot" {
		t.Fatalf("imageRaw = %q, want registry ref", got.imageRaw)
	}
	if got.registryURL != "https://registry.example.com" {
		t.Fatalf("registryURL = %q, want registry.example.com", got.registryURL)
	}
	if got.image != "base-snapshot" {
		t.Fatalf("image = %q, want base-snapshot", got.image)
	}
	if got.runImage() != "https://registry.example.com/base-snapshot" {
		t.Fatalf("runImage = %q, want raw image ref", got.runImage())
	}
	if got.cloneImage() != "base-snapshot" {
		t.Fatalf("cloneImage = %q, want base-snapshot", got.cloneImage())
	}
	if got.storage != "40G" || got.nics != "2" || got.dns != "1.1.1.1,8.8.8.8" || got.rootPwd != "secret" || got.osType != "windows" {
		t.Fatalf("unexpected resolved spec: %#v", got)
	}
}

func TestResolvePodSpecDefaultsFromContainerImage(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "ubuntu-dev-base"}},
		},
	}

	got := resolvePodSpec(pod)
	if got.imageRaw != "ubuntu-dev-base" {
		t.Fatalf("imageRaw = %q, want ubuntu-dev-base", got.imageRaw)
	}
	if got.registryURL != "" {
		t.Fatalf("registryURL = %q, want empty", got.registryURL)
	}
	if got.image != "ubuntu-dev-base" {
		t.Fatalf("image = %q, want ubuntu-dev-base", got.image)
	}
	if got.storage != defaultLinuxStorage || got.nics != defaultNICs || got.osType != defaultOSType {
		t.Fatalf("defaults not applied: %#v", got)
	}
}

func TestResolvePodSpecWindowsDefaults(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnOS: "windows",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "win11-base"}},
		},
	}

	got := resolvePodSpec(pod)
	if got.osType != osWindows {
		t.Fatalf("osType = %q, want %q", got.osType, osWindows)
	}
	if got.storage != defaultWindowsStorage {
		t.Fatalf("storage = %q, want %q", got.storage, defaultWindowsStorage)
	}
}

func TestStorePodVMStoresDeepCopyAndAnnotations(t *testing.T) {
	p := newTestProvider()
	pod := newTestPod("app", map[string]string{
		AnnMode: "clone",
	})
	vm := &CocoonVM{
		vmID:    "vm-1",
		vmName:  "vk-testns-app",
		ip:      "10.88.100.23",
		mac:     "02:00:00:00:00:23",
		managed: true,
	}

	p.storePodVM(nil, podKey(pod.Namespace, pod.Name), pod, vm, podAnnotation{key: AnnSnapshotFrom, value: "snapshot-a"})

	key := podKey(pod.Namespace, pod.Name)
	storedPod := p.pods[key]
	if storedPod == nil {
		t.Fatalf("expected stored pod")
	}
	if got := storedPod.Annotations[AnnVMID]; got != "vm-1" {
		t.Fatalf("stored VMID annotation = %q, want vm-1", got)
	}
	if got := storedPod.Annotations[AnnSnapshotFrom]; got != "snapshot-a" {
		t.Fatalf("stored snapshot annotation = %q, want snapshot-a", got)
	}
	pod.Annotations[AnnVMID] = "vm-mutated"
	if got := p.pods[key].Annotations[AnnVMID]; got != "vm-1" {
		t.Fatalf("stored pod mutated with caller changes: %q", got)
	}
	if p.vms[key] != vm {
		t.Fatalf("stored VM pointer mismatch")
	}
}

func TestMergeVMDetails(t *testing.T) {
	dst := &CocoonVM{
		vmID:     "vm-old",
		vmName:   "old-name",
		ip:       "10.0.0.1",
		mac:      "00:11:22:33:44:55",
		state:    stateCreating,
		image:    "old-image",
		os:       "linux",
		managed:  false,
		cpu:      2,
		memoryMB: 4096,
	}
	src := &CocoonVM{
		vmID:      "vm-new",
		vmName:    "new-name",
		ip:        "10.0.0.2",
		state:     stateRunning,
		image:     "new-image",
		os:        "windows",
		managed:   true,
		cpu:       4,
		memoryMB:  8192,
		createdAt: metav1.Now().Time,
		startedAt: metav1.Now().Time,
	}

	mergeVMDetails(dst, src)

	if dst.vmID != "vm-new" || dst.vmName != "new-name" || dst.ip != "10.0.0.2" || dst.state != stateRunning {
		t.Fatalf("details not merged correctly: %#v", dst)
	}
	if dst.image != "new-image" || dst.os != "windows" || !dst.managed || dst.cpu != 4 || dst.memoryMB != 8192 {
		t.Fatalf("resource fields not merged correctly: %#v", dst)
	}
	if dst.createdAt.IsZero() || dst.startedAt.IsZero() {
		t.Fatalf("timestamps were not merged: %#v", dst)
	}
}
