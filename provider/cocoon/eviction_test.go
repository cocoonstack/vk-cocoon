package cocoon

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestVMGoneEvictionContinuesPastInspectBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
		cs := fake.NewSimpleClientset(pod)
		deletes := 0
		cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			deletes++
			if deletes <= 4 {
				return true, nil, errors.New("api server unreachable")
			}
			return false, nil, nil
		})
		p := newTestProvider(t)
		p.Runtime = &fakeRuntime{}
		p.Clientset = cs
		p.deferredRecheckInitialDelay = time.Millisecond
		p.deferredRecheckMaxDelay = 2 * time.Millisecond
		p.deferredRecheckBudget = time.Millisecond
		p.trackPod(pod, &vm.VM{ID: "vmid-evict", Name: "vk-ns-demo-0"})

		p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-evict", Name: "vk-ns-demo-0"})
		time.Sleep(time.Second)
		synctest.Wait()
		if got := p.vmForPod("ns", "demo-0"); got != nil {
			t.Fatalf("pod still tracked after API recovery beyond the inspect budget: %#v", got)
		}
	})
}

func TestVMRemovalEvictionRetriesAfterAPIRecovery(t *testing.T) {
	for _, path := range []string{"watch event", "deferred restart", "inspect timeout"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
				stopped := &vm.VM{ID: "vmid-evict", Name: "vk-ns-demo-0", State: "stopped"}
				cs := fake.NewSimpleClientset(pod)
				deletes := 0
				cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					deletes++
					if deletes <= 4 {
						return true, nil, errors.New("api server unreachable")
					}
					return false, nil, nil
				})
				p := newTestProvider(t)
				rt := &fakeRuntime{inspectVM: stopped}
				p.Runtime = rt
				p.Clientset = cs
				p.inlineInspectBaseDelay = time.Millisecond
				p.deferredRecheckInitialDelay = time.Millisecond
				p.deferredRecheckMaxDelay = 2 * time.Millisecond
				p.deferredRecheckBudget = time.Millisecond
				p.trackPod(pod, stopped)
				p.lastRestart[stopped.ID] = time.Now()
				rt.onRemove = func() {
					rt.inspectVM, rt.inspectErr = nil, nil
					p.handleVMGone(t.Context(), stopped)
				}
				switch path {
				case "deferred restart":
					rt.inspectSeq = []fakeInspectStep{{err: errors.New("broken pipe")}, {err: errors.New("broken pipe")}}
				case "inspect timeout":
					rt.inspectErr = errors.New("broken pipe")
				}

				p.handleVMGone(t.Context(), stopped)
				time.Sleep(time.Second)
				synctest.Wait()
				if got := p.vmForPod("ns", "demo-0"); got != nil {
					t.Fatalf("pod still tracked after VM removal and API recovery: %#v", got)
				}
			})
		})
	}
}
