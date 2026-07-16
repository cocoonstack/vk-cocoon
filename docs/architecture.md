# Architecture

vk-cocoon is the host-side bridge between the Kubernetes API and the
cocoon runtime running on a single node. It satisfies the
[virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet)
provider contract by translating pod CRUD into cocoon CLI calls and
reporting per-VM status back to the kubelet.

## Layer map

| Layer | Package | Responsibility |
|---|---|---|
| Application | `package main` | Entry point, node registration, metrics server, VM event watcher startup |
| Provider | `provider/cocoon/` | `Provider` struct with lifecycle methods (CreatePod / DeletePod / UpdatePod / GetPodStatus), startup reconcile, orphan policy, VM event watcher, pod eviction |
| Provider iface | `provider/` | Shared provider interface and node-capacity helpers |
| Cocoon CLI | `vm/` | `Runtime` interface + the default `CocoonCLI` implementation that shells out to `cocoon` (including `WatchEvents` via `cocoon vm status --event --format json`) |
| Snapshot SDK | `snapshots/` | `Puller` and `Pusher` stream snapshots and cloud images to any OCI registry through `cocoon-common`'s `oci.Registry` backend (`cocoon-common/snapshot` + `cocoon-common/cloudimg`) |
| Network | `network/` | cocoon-net JSON lease parser used to resolve a freshly cloned VM's IP, plus the ICMPv4 `Pinger` the probe loop uses to check guest reachability |
| Guest console | `guest/` | SAC dialer (Windows static IP). Guest exec / logs go through `cocoon vm exec` and `cocoon vm logs` — see `vm/` |
| Probes | `probes/` | Per-pod probe agents that run a caller-supplied health check on a ticker, update the in-memory readiness map, and invoke an onUpdate callback so the async provider can push fresh status through v-k's notify hook |
| Metrics | `metrics/` | Prometheus collectors for pod lifecycle, snapshot pull / push, VM table size, orphans |
| Build metadata | `version/` | ldflags-injected version / revision / built-at strings |

## The async-provider contract

vk-cocoon implements virtual-kubelet's `NotifyPods` interface, so the
framework treats it as an **async provider**: Kubernetes only sees the
pod status vk-cocoon actively pushes through `notify`, and v-k never
polls `GetPodStatus` on its own.

That single fact shapes the rest of the design. Any status change that
happens after `CreatePod` returns is invisible to the cluster unless
vk-cocoon re-fires `notify`, so two independent mechanisms keep the
pushed status honest:

- a per-pod [probe loop](probes.md) that re-checks guest reachability on
  a ticker and re-notifies when readiness flips, and
- a real-time [VM event watcher](reconcile.md) that reacts to cocoon's
  own state stream within a second.

## Why shell out to cocoon

cocoon is the authoritative VM controller and exposes its contract
through its CLI. vk-cocoon shells out (`vm/`) rather than linking against
cocoon's internals — the subprocess boundary is architectural, not tech
debt. Per-VM stats are the one exception: they are read from `/proc`
using the hypervisor PID tracked in memory, so a metrics scrape never
forks a `cocoon` process.

## Place in the cocoonstack

| Project | Role |
|---|---|
| [cocoon](https://github.com/cocoonstack/cocoon) | The MicroVM runtime vk-cocoon shells out to |
| [cocoon-common](https://github.com/cocoonstack/cocoon-common) | CRD types, annotation contract, shared helpers, and the OCI registry + snapshot/cloud-image packages |
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and CocoonHibernation reconcilers |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Admission webhook for sticky scheduling and CocoonSet validation |
| [cocoon-net](https://github.com/cocoonstack/cocoon-net) | Per-host networking with embedded DHCP server and iptables setup; vk-cocoon reads its JSON lease file |
