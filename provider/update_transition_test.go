package provider

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestUpdatePodTriggersHibernateTransition(t *testing.T) {
	p := newTestProvider()
	key := podKey("testns", "app")
	p.pods[key] = newTestPod("app", nil)
	p.vms[key] = &CocoonVM{
		vmName: "vk-testns-app-1",
		state:  stateRunning,
	}

	triggered := make(chan string, 1)
	p.hibernateVMFn = func(_ context.Context, pod *corev1.Pod, vm *CocoonVM) {
		triggered <- ann(pod, AnnHibernate, "")
	}

	if err := p.UpdatePod(context.Background(), newTestPod("app", map[string]string{
		AnnHibernate: valTrue,
	})); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}

	select {
	case got := <-triggered:
		if got != valTrue {
			t.Fatalf("hibernate trigger annotation = %q, want %q", got, valTrue)
		}
	case <-time.After(time.Second):
		t.Fatal("expected hibernate transition to trigger")
	}
}

func TestUpdatePodTriggersWakeTransition(t *testing.T) {
	p := newTestProvider()
	key := podKey("testns", "app")
	p.pods[key] = newTestPod("app", map[string]string{
		AnnHibernate: valTrue,
	})
	p.vms[key] = &CocoonVM{
		vmName: "vk-testns-app-1",
		state:  stateHibernated,
	}

	triggered := make(chan string, 1)
	p.wakeVMFn = func(_ context.Context, pod *corev1.Pod, vm *CocoonVM) {
		triggered <- ann(pod, AnnHibernate, "")
	}

	if err := p.UpdatePod(context.Background(), newTestPod("app", nil)); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}

	select {
	case got := <-triggered:
		if got != "" {
			t.Fatalf("wake trigger hibernate annotation = %q, want empty", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected wake transition to trigger")
	}
}
