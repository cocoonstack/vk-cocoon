package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// OrphanPolicy controls what happens to VMs with no matching pod at startup reconcile.
type OrphanPolicy string

const (
	OrphanAlert   OrphanPolicy = "alert"
	OrphanDestroy OrphanPolicy = "destroy"
	OrphanKeep    OrphanPolicy = "keep"
)

// CocoonProvider maps Kubernetes pods to cocoon MicroVMs.
type CocoonProvider struct {
	NodeName string

	Clientset    kubernetes.Interface
	Runtime      vm.Runtime
	Puller       *snapshots.Puller
	Pusher       *snapshots.Pusher
	Registry     snapshots.RegistryClient
	LeaseParser  *network.LeaseParser
	Pinger       network.Pinger
	GuestSSH     *guest.SSHExecutor
	GuestRDP     guest.RDPExecutor
	Probes       *probes.Manager
	OrphanPolicy OrphanPolicy

	mu         sync.RWMutex
	pods       map[string]*corev1.Pod
	vmsByPod   map[string]*vm.VM
	vmsByName  map[string]*vm.VM
	notifyHook func(*corev1.Pod)
}

// NewCocoonProvider constructs a CocoonProvider with empty tables.
// Default Pinger is NopPinger so tests degrade gracefully.
func NewCocoonProvider() *CocoonProvider {
	return &CocoonProvider{
		OrphanPolicy: OrphanAlert,
		Pinger:       network.NopPinger{},
		pods:         map[string]*corev1.Pod{},
		vmsByPod:     map[string]*vm.VM{},
		vmsByName:    map[string]*vm.VM{},
	}
}

// GetPod returns a deep copy of the stored pod.
func (p *CocoonProvider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[meta.PodKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	return pod.DeepCopy(), nil
}

// GetPods returns every pod the provider owns.
func (p *CocoonProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Collect(maps.Values(p.pods)), nil
}

// NotifyPods stores the kubelet's pod-status callback.
func (p *CocoonProvider) NotifyPods(_ context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifyHook = notifier
}

// notify pushes a pod status update through the kubelet callback.
func (p *CocoonProvider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	hook := p.notifyHook
	p.mu.RUnlock()
	if hook != nil {
		hook(pod)
	}
}

// trackPod stores the pod and its VM in the in-memory tables.
func (p *CocoonProvider) trackPod(pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.pods[key] = pod
	if v != nil {
		p.vmsByPod[key] = v
		if v.Name != "" {
			p.vmsByName[v.Name] = v
		}
	}
}

// dropVMLocked removes the VM record for key. Caller must hold p.mu for writing.
func (p *CocoonProvider) dropVMLocked(key string) {
	v, ok := p.vmsByPod[key]
	if !ok {
		return
	}
	if v.Name != "" {
		delete(p.vmsByName, v.Name)
	}
	delete(p.vmsByPod, key)
}

// forgetPod drops the pod and VM from the in-memory tables.
func (p *CocoonProvider) forgetPod(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(namespace, name)
	p.dropVMLocked(key)
	delete(p.pods, key)
}

// vmForPod returns the VM associated with a pod, or nil.
func (p *CocoonProvider) vmForPod(namespace, name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByPod[meta.PodKey(namespace, name)]
}

// setVMIP updates the tracked VM's IP (copy-on-write for concurrency safety).
func (p *CocoonProvider) setVMIP(namespace, name, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(namespace, name)
	v, ok := p.vmsByPod[key]
	if !ok {
		return
	}
	updated := *v
	updated.IP = ip
	p.vmsByPod[key] = &updated
	if updated.Name != "" {
		p.vmsByName[updated.Name] = &updated
	}
}

// resolveVMIP returns the VM's IP, falling back to a dnsmasq lease lookup
// when the IP is unknown but a MAC is available.
func (p *CocoonProvider) resolveVMIP(namespace, name string, v *vm.VM) string {
	ip := v.IP
	if ip != "" || v.MAC == "" || p.LeaseParser == nil {
		return ip
	}
	lease, err := p.LeaseParser.LookupByMAC(v.MAC)
	if err != nil {
		return ""
	}
	p.setVMIP(namespace, name, lease.IP)
	return lease.IP
}

// buildProbe returns a probe closure that resolves the VM's IP and pings it.
// ICMP works for both Linux and Windows guests.
func (p *CocoonProvider) buildProbe(namespace, name string) probes.Probe {
	return func(ctx context.Context) (bool, string) {
		v := p.vmForPod(namespace, name)
		if v == nil {
			return false, "vm gone"
		}
		ip := p.resolveVMIP(namespace, name, v)
		if ip == "" {
			return false, "waiting for dhcp lease"
		}
		if err := p.Pinger.Ping(ctx, ip); err != nil {
			return false, "ping failed: " + err.Error()
		}
		return true, "ping ok"
	}
}

// buildOnUpdate returns the callback invoked on readiness transitions.
// Under the async provider contract this is the only way status changes reach the kubelet.
func (p *CocoonProvider) buildOnUpdate(namespace, name string) probes.OnUpdate {
	return func(ctx context.Context) {
		pod, err := p.GetPod(ctx, namespace, name)
		if err != nil {
			log.WithFunc("CocoonProvider.probeUpdate").
				Warnf(ctx, "pod %s/%s lookup failed, skipping notify: %v", namespace, name, err)
			return
		}
		p.refreshStatus(ctx, pod)
		p.notify(pod)
	}
}
