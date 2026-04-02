package provider

import "testing"

func TestStoreUpdatedPodPreservesManagedAnnotationsOnStoredCopy(t *testing.T) {
	p := newTestProvider()
	key := podKey("testns", "app")

	p.pods[key] = newTestPod("app", map[string]string{
		AnnVMName:       "vk-testns-app-0",
		AnnVMID:         "vm-123",
		AnnIP:           "10.88.100.23",
		AnnManaged:      valTrue,
		AnnSnapshotFrom: "snapshot-a",
	})

	incoming := newTestPod("app", map[string]string{
		AnnHibernate: valTrue,
	})
	stored := p.storeUpdatedPod(key, incoming)

	if got := stored.Annotations[AnnVMID]; got != "vm-123" {
		t.Fatalf("stored VMID = %q, want vm-123", got)
	}
	if got := stored.Annotations[AnnSnapshotFrom]; got != "snapshot-a" {
		t.Fatalf("stored snapshot = %q, want snapshot-a", got)
	}
	if _, ok := incoming.Annotations[AnnVMID]; ok {
		t.Fatalf("incoming pod was mutated with provider-managed annotations")
	}
	if got := p.pods[key].Annotations[AnnHibernate]; got != valTrue {
		t.Fatalf("stored hibernate annotation = %q, want %q", got, valTrue)
	}
}

func TestStoreUpdatedPodStoresDeepCopyWithoutMutatingInput(t *testing.T) {
	p := newTestProvider()
	key := podKey("testns", "app")

	incoming := newTestPod("app", nil)
	stored := p.storeUpdatedPod(key, incoming)

	if stored == incoming {
		t.Fatalf("stored pod should be a deep copy")
	}
	if p.pods[key] == nil {
		t.Fatalf("expected stored pod state")
	}
	incoming.Labels = map[string]string{"changed": "true"}
	if p.pods[key].Labels["changed"] == "true" {
		t.Fatalf("stored pod should not track caller mutations")
	}
}
