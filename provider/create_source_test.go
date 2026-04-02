package provider

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestResolveCloneSourcePrefersSuspendedSnapshot(t *testing.T) {
	vmName := "vk-testns-demo-0"
	p := newTestProvider()
	p.lookupSuspendedSnapshotFn = nil
	p.kubeClient = k8sfake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      suspendedSnapshotConfigMap,
			Namespace: "testns",
		},
		Data: map[string]string{
			vmName: "https://epoch.local/" + vmName + "-suspend",
		},
	})

	pod := newTestPod("demo", map[string]string{
		AnnMode:  modeClone,
		AnnImage: "base-image",
	})
	req := newCreateRequest(pod)

	cloneImage, registryURL := p.resolveCloneSource(context.Background(), req, vmName)
	if cloneImage != vmName+"-suspend" {
		t.Fatalf("cloneImage = %q, want %q", cloneImage, vmName+"-suspend")
	}
	if registryURL != "https://epoch.local" {
		t.Fatalf("registryURL = %q, want https://epoch.local", registryURL)
	}

	cm, err := p.kubeClient.CoreV1().ConfigMaps("testns").Get(context.Background(), suspendedSnapshotConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get snapshots configmap: %v", err)
	}
	if got := cm.Data[vmName]; got != "" {
		t.Fatalf("suspended snapshot entry = %q, want cleared for slot-0", got)
	}
}

func TestResolveCloneSourcePrefersAnnotatedForkSourceOverMainAgent(t *testing.T) {
	p := newTestProvider()
	p.vms["source"] = &CocoonVM{vmName: "toolbox-vm", vmID: "vm-toolbox", state: stateRunning}
	p.vms["main"] = &CocoonVM{vmName: "vk-testns-demo-0", vmID: "vm-main", state: stateRunning}

	var calls [][]string
	p.cocoonExecFn = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "ok", nil
	}

	pod := newTestPod("demo-1", map[string]string{
		AnnMode:     modeClone,
		AnnImage:    "base-image",
		AnnForkFrom: "toolbox-vm",
	})
	req := newCreateRequest(pod)

	cloneImage, _ := p.resolveCloneSource(context.Background(), req, "vk-testns-demo-1")
	if cloneImage != "vk-testns-demo-1-fork" {
		t.Fatalf("cloneImage = %q, want vk-testns-demo-1-fork", cloneImage)
	}
	if len(calls) < 2 {
		t.Fatalf("expected snapshot save calls, got %d", len(calls))
	}
	last := strings.Join(calls[len(calls)-1], " ")
	if !strings.Contains(last, "vm-toolbox") {
		t.Fatalf("fork should use annotated source VM, got %q", last)
	}
	if strings.Contains(last, "vm-main") {
		t.Fatalf("fork should not use main agent when annotation is present, got %q", last)
	}
}
