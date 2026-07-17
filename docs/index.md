# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to
[Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs. One vk-cocoon
process runs per node, translating pod CRUD into `cocoon` CLI calls and
pushing per-VM status back to the kubelet.

```
Kubernetes API ──► virtual-kubelet provider (vk-cocoon, one per node)
   pod CRUD    ──► CreatePod / DeletePod / UpdatePod ── cocoon clone/run/snapshot
   status      ◄── async notify ── per-pod probe loop + real-time VM event watcher
   snapshots   ──► Puller / Pusher ── OCI registry (cross-node hibernate/wake)
```

## Guides

- [Architecture](architecture.md) — the layer map, the async-provider
  contract, and where vk-cocoon sits in the cocoonstack
- [Pod lifecycle](lifecycle.md) — CreatePod (clone / run / static),
  DeletePod (snapshot-on-destroy), and the hibernate/wake UpdatePod path
- [Readiness probing](probes.md) — the per-pod probe loop that makes an
  async provider's status live (ICMP / TCP, IP re-resolution)
- [Runtime reconciliation](reconcile.md) — startup reconcile, orphan
  policy, and the sub-second VM event watcher
- [Post-clone network hints](post-clone.md) — when a cloned guest needs
  manual network fixup and how the hint annotation surfaces it
- [Node resources](node-resources.md) — host-probed Capacity /
  Allocatable and the override knobs
- [Metrics & monitoring](metrics.md) — the kubelet stats API, the
  metrics-server resource endpoint, and the `cocoon_vk_*` families
- [Configuration](configuration.md) — every environment variable
- [Installation](installation.md) — the systemd unit, capabilities, and
  building from source

## Repository

Source and issue tracker:
[github.com/cocoonstack/vk-cocoon](https://github.com/cocoonstack/vk-cocoon).
Part of the [cocoonstack](https://cocoonstack.github.io/) MicroVM platform.
