package main

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// CocoonProvider is the virtual-kubelet provider that maps
// Kubernetes pods to cocoon MicroVMs. It owns the in-memory pod
// table and the dependencies the per-feature files (pods_create,
// pods_delete, etc.) operate against.
type CocoonProvider struct {
	NodeName string

	Clientset    kubernetes.Interface
	Runtime      vm.Runtime
	Puller       *snapshots.Puller
	Pusher       *snapshots.Pusher
	Registry     snapshots.RegistryClient
	LeaseParser  *network.LeaseParser
	GuestSSH     *guest.SSHExecutor
	GuestRDP     guest.RDPExecutor
	Probes       *probes.Manager
	OrphanPolicy OrphanPolicy

	mu         sync.RWMutex
	pods       map[string]*corev1.Pod
	vmsByPod   map[string]*vm.VM
	vmsByID    map[string]*vm.VM
	vmsByName  map[string]*vm.VM
	notifyHook func(*corev1.Pod)
}

// OrphanPolicy controls what vk-cocoon does when startup reconcile
// finds a VM with no matching pod.
type OrphanPolicy string

const (
	// OrphanAlert logs and increments a metric counter but leaves
	// the VM alone. The default; safest.
	OrphanAlert OrphanPolicy = "alert"
	// OrphanDestroy removes the VM. Aggressive; opt-in.
	OrphanDestroy OrphanPolicy = "destroy"
	// OrphanKeep is a no-op (no log, no metric).
	OrphanKeep OrphanPolicy = "keep"
)

// NewCocoonProvider constructs a CocoonProvider with empty in-memory
// tables. Callers fill in the dependencies, then call StartupReconcile
// once to populate the tables from the live cluster + cocoon state.
func NewCocoonProvider() *CocoonProvider {
	return &CocoonProvider{
		OrphanPolicy: OrphanAlert,
		pods:         map[string]*corev1.Pod{},
		vmsByPod:     map[string]*vm.VM{},
		vmsByID:      map[string]*vm.VM{},
		vmsByName:    map[string]*vm.VM{},
	}
}

// podKey is the canonical "<namespace>/<name>" key vk-cocoon uses
// to index its in-memory tables.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// GetPod returns the pod previously stored for the given key.
func (p *CocoonProvider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[podKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	return pod, nil
}

// GetPods returns every pod the provider currently owns.
func (p *CocoonProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		out = append(out, pod)
	}
	return out, nil
}

// NotifyPods stores the callback the kubelet uses to receive pod
// status updates from the provider.
func (p *CocoonProvider) NotifyPods(_ context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifyHook = notifier
}

// notify pushes a pod status update through the kubelet callback,
// if one is registered.
func (p *CocoonProvider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	hook := p.notifyHook
	p.mu.RUnlock()
	if hook != nil {
		hook(pod)
	}
}

// trackPod stores the pod and (optionally) its associated VM in
// the in-memory tables.
func (p *CocoonProvider) trackPod(pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := podKey(pod.Namespace, pod.Name)
	p.pods[key] = pod
	if v != nil {
		p.vmsByPod[key] = v
		if v.ID != "" {
			p.vmsByID[v.ID] = v
		}
		if v.Name != "" {
			p.vmsByName[v.Name] = v
		}
	}
}

// forgetPod drops the pod and associated VM from the in-memory
// tables. Used by DeletePod and the orphan reconcile loop.
func (p *CocoonProvider) forgetPod(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := podKey(namespace, name)
	if v, ok := p.vmsByPod[key]; ok {
		delete(p.vmsByID, v.ID)
		delete(p.vmsByName, v.Name)
		delete(p.vmsByPod, key)
	}
	delete(p.pods, key)
}

// vmForPod returns the VM record currently associated with a pod,
// or nil when none has been recorded.
func (p *CocoonProvider) vmForPod(namespace, name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByPod[podKey(namespace, name)]
}
