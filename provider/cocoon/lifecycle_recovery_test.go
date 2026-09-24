package cocoon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestHibernateFailureRefreshesRuntime(t *testing.T) {
	for _, stage := range []string{"save", "push", "remove", "canceled save"} {
		for _, network := range []string{"dhcp", "static"} {
			t.Run(stage+"/"+network, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				failure := errors.New("hibernate failed")
				v := &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0-505043", IP: "192.0.2.7", MAC: "02:00:00:00:00:01"}
				fresh := &vm.VM{ID: v.ID, Name: v.Name, IP: "192.0.2.8", MAC: "02:00:00:00:00:02"}
				fresh.NetworkConfigs = []*vm.NetworkConfig{{MAC: fresh.MAC}}
				wantIP := ""
				if network == "static" {
					v.NetworkConfigs = []*vm.NetworkConfig{{MAC: v.MAC, Network: &vm.NetworkInfo{IP: v.IP}}}
					fresh.NetworkConfigs[0].Network = &vm.NetworkInfo{IP: fresh.IP}
					wantIP = fresh.IP
				}
				rt := &hibernateRollbackRuntime{fakeRuntime: &fakeRuntime{inspectVM: fresh}}
				p := newTestProvider(t)
				p.Runtime = rt
				switch stage {
				case "save":
					rt.snapshotSaveErr = failure
				case "push":
					rt.exportErr = failure
					p.Pusher = &snapshots.Pusher{Runtime: rt, Registry: fakeRegistry{}}
				case "remove":
					rt.removeErr = failure
				case "canceled save":
					rt.snapshotSaveHook = cancel
					failure = context.Canceled
				}
				pod := newPodWithSpec(meta.VMSpec{VMName: v.Name, Backend: string(cocoonv1.BackendCloudHypervisor), OS: string(cocoonv1.OSWindows)})
				pod.UID = "original"
				meta.VMRuntime{VMID: v.ID, IP: v.IP}.Apply(pod)
				p.Clientset = fake.NewSimpleClientset(pod.DeepCopy())
				p.trackPod(pod, v)

				if err := p.hibernate(ctx, pod, meta.ParseVMSpec(pod), v); !errors.Is(err, failure) {
					t.Fatalf("hibernate error = %v, want %v", err, failure)
				}
				tracked := p.vmForPod(pod.Namespace, pod.Name)
				if tracked == nil || tracked.MAC != fresh.MAC || tracked.IP != wantIP || !reflect.DeepEqual(tracked.NetworkConfigs, fresh.NetworkConfigs) {
					t.Fatalf("tracked VM = %#v, want current NIC and IP %q", tracked, wantIP)
				}
				published, err := p.Clientset.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get pod: %v", err)
				}
				if published.Annotations[meta.AnnotationVMID] != v.ID || published.Annotations[meta.AnnotationIP] != wantIP {
					t.Errorf("published runtime = %v, want VMID %q and IP %q", published.Annotations, v.ID, wantIP)
				}
				if got := execArgvs(rt.fakeRuntime); !slices.Equal(got, []string{"cmd /c ipconfig /release", "cmd /c ipconfig /renew"}) {
					t.Errorf("guest commands = %v, want release then renew", got)
				}
				if network == "dhcp" {
					p.LeaseParser = newLeaseParser(t, fresh.MAC, fresh.IP)
					if got := p.resolveVMIP(pod.Namespace, pod.Name, tracked); got != fresh.IP {
						t.Errorf("resolved IP = %q, want the replacement NIC lease %q", got, fresh.IP)
					}
				}
			})
		}
	}
}

func TestDeletePodRetryCompletesWhenVMIsAlreadyGone(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep-snapshots=%t", keep), func(t *testing.T) {
			rt := &fakeRuntime{removeErr: context.DeadlineExceeded}
			releaser := &recordingLeaseReleaser{}
			p := newTestProvider(t)
			p.Runtime, p.LeaseReleaser = rt, releaser
			pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})
			if keep {
				meta.MarkKeepSnapshotOnDelete(pod)
			}
			v := &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0-505043", MAC: "02:00:00:00:00:01"}
			p.trackPod(pod, v)
			key := meta.PodKey(pod.Namespace, pod.Name)
			p.Probes.Set(key, probes.Result{Ready: true})
			var phase corev1.PodPhase
			p.notifyHook = func(pod *corev1.Pod) { phase = pod.Status.Phase }

			if err := p.DeletePod(t.Context(), pod); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first delete = %v, want the real removal error", err)
			}
			if p.vmForPod(pod.Namespace, pod.Name) == nil || len(releaser.released()) != 0 || len(rt.snapshotRemoveCalls) != 0 {
				t.Fatal("inconclusive removal discarded the tracked VM or released resources")
			}
			rt.removeErr = fmt.Errorf("remove interrupted VM: %w", vm.ErrVMNotFound)
			if err := p.DeletePod(t.Context(), pod); err != nil {
				t.Fatalf("retry delete: %v", err)
			}
			if _, _, tracked := p.trackedIncarnation(key); tracked || p.vmForPod(pod.Namespace, pod.Name) != nil || p.Probes.Get(key).Ready {
				t.Fatal("successful deletion retained pod, VM, or probe state")
			}
			if phase != corev1.PodSucceeded {
				t.Errorf("published phase = %q, want Succeeded", phase)
			}
			var wantSnapshots []string
			if !keep {
				wantSnapshots = []string{forkSnapshotName(v.Name), v.Name}
			}
			if got := slices.Sorted(slices.Values(rt.snapshotRemoveCalls)); !slices.Equal(got, wantSnapshots) {
				t.Errorf("removed snapshots = %v, want %v", got, wantSnapshots)
			}
			if err := p.DeletePod(t.Context(), pod); err != nil {
				t.Fatalf("repeated delete: %v", err)
			}
			if got := releaser.released(); !slices.Equal(got, []string{v.MAC}) {
				t.Errorf("released leases = %v, want one release for %q", got, v.MAC)
			}
		})
	}
}

type hibernateRollbackRuntime struct {
	*fakeRuntime
	exportErr error
}

func (r *hibernateRollbackRuntime) Inspect(ctx context.Context, id string) (*vm.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.fakeRuntime.Inspect(ctx, id)
}

func (r *hibernateRollbackRuntime) SnapshotExport(context.Context, string) (io.ReadCloser, func() error, error) {
	return nil, nil, r.exportErr
}
