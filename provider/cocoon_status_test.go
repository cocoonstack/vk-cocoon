package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func newTestProviderWithClient(objs ...runtime.Object) *CocoonProvider {
	p := newTestProvider()
	p.nodeIP = "192.0.2.10"
	p.kubeClient = k8sfake.NewSimpleClientset(objs...)
	return p
}

func TestGetPodStatusUsesNodeIPAndRefreshesGuestIP(t *testing.T) {
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "testns",
			Name:      "windows-vm",
			Annotations: map[string]string{
				AnnIP: "10.88.1.207",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "vm"}},
		},
	}

	p := newTestProviderWithClient(livePod)
	key := podKey("testns", "windows-vm")
	p.pods[key] = livePod.DeepCopy()
	p.vms[key] = &CocoonVM{
		podNamespace: "testns",
		podName:      "windows-vm",
		vmID:         "vm-1",
		vmName:       "windows-vm",
		ip:           "10.88.1.207",
		state:        "running",
		os:           "windows",
		managed:      true,
	}
	p.discoverVMByIDFn = func(_ context.Context, vmID string) *CocoonVM {
		if vmID != "vm-1" {
			t.Fatalf("discoverVMByID got %q, want vm-1", vmID)
		}
		return &CocoonVM{
			vmID:   "vm-1",
			vmName: "windows-vm",
			ip:     "10.88.100.21",
			mac:    "ea:a8:39:28:63:1c",
			state:  "running",
		}
	}

	status, err := p.GetPodStatus(context.Background(), "testns", "windows-vm")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if got := status.HostIP; got != "192.0.2.10" {
		t.Fatalf("HostIP = %q, want 192.0.2.10", got)
	}
	if got := status.PodIP; got != "10.88.100.21" {
		t.Fatalf("PodIP = %q, want 10.88.100.21", got)
	}
	if got := p.vms[key].ip; got != "10.88.100.21" {
		t.Fatalf("cached VM IP = %q, want 10.88.100.21", got)
	}

	patched, err := p.kubeClient.CoreV1().Pods("testns").Get(context.Background(), "windows-vm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get patched pod: %v", err)
	}
	if got := patched.Annotations[AnnIP]; got != "10.88.100.21" {
		t.Fatalf("patched annotation IP = %q, want 10.88.100.21", got)
	}
	if got := patched.Annotations[AnnMAC]; got != "ea:a8:39:28:63:1c" {
		t.Fatalf("patched annotation MAC = %q, want ea:a8:39:28:63:1c", got)
	}
}

func TestReconcileOnceBackfillsGuestIPIntoPodAnnotations(t *testing.T) {
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "testns",
			Name:      "linux-vm",
			Annotations: map[string]string{
				AnnIP: "10.88.1.196",
			},
		},
	}

	p := newTestProviderWithClient(livePod)
	key := podKey("testns", "linux-vm")
	p.pods[key] = livePod.DeepCopy()
	p.vms[key] = &CocoonVM{
		podNamespace: "testns",
		podName:      "linux-vm",
		vmID:         "vm-2",
		vmName:       "linux-vm",
		ip:           "10.88.1.196",
		state:        "running",
		managed:      true,
	}
	p.discoverVMByIDFn = func(_ context.Context, vmID string) *CocoonVM {
		if vmID != "vm-2" {
			t.Fatalf("discoverVMByID got %q, want vm-2", vmID)
		}
		return &CocoonVM{
			vmID:   "vm-2",
			vmName: "linux-vm",
			ip:     "10.88.100.207",
			mac:    "0a:3c:8d:80:34:f7",
			state:  "running",
		}
	}

	p.reconcileOnce()

	if got := p.vms[key].ip; got != "10.88.100.207" {
		t.Fatalf("cached VM IP = %q, want 10.88.100.207", got)
	}
	patched, err := p.kubeClient.CoreV1().Pods("testns").Get(context.Background(), "linux-vm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get patched pod: %v", err)
	}
	if got := patched.Annotations[AnnIP]; got != "10.88.100.207" {
		t.Fatalf("patched annotation IP = %q, want 10.88.100.207", got)
	}
	if got := patched.Annotations[AnnMAC]; got != "0a:3c:8d:80:34:f7" {
		t.Fatalf("patched annotation MAC = %q, want 0a:3c:8d:80:34:f7", got)
	}
}
