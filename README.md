# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to [Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs.

## Overview

vk-cocoon is the host-side bridge between the Kubernetes API and the cocoon runtime running on a single node. It satisfies the [virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider contract by translating pod CRUD into cocoon CLI calls and reporting per-VM status back to the kubelet.

| Layer | Package | Responsibility |
|---|---|---|
| Application | `package main` | Entry point, node registration, metrics server, VM event watcher startup |
| Provider | `provider/cocoon/` | `Provider` struct with lifecycle methods (CreatePod / DeletePod / UpdatePod / GetPodStatus), startup reconcile, orphan policy, VM event watcher, pod eviction |
| Provider iface | `provider/` | Shared provider interface and node-capacity helpers |
| Cocoon CLI | `vm/` | `Runtime` interface + the default `CocoonCLI` implementation that shells out to `sudo cocoon …` (including `WatchEvents` via `cocoon vm status --event --format json`) |
| Snapshot SDK | `snapshots/` | Wraps the [epoch](https://github.com/cocoonstack/epoch) SDK as a `RegistryClient` interface, plus `Puller` and `Pusher` that stream snapshots and cloud images via `epoch/snapshot` and `epoch/cloudimg` |
| Network | `network/` | cocoon-net JSON lease parser used to resolve a freshly cloned VM's IP, plus the ICMPv4 `Pinger` the probe loop uses to check guest reachability |
| Guest exec | `guest/` | SSH executor (Linux) and RDP help-text shim (Windows) |
| Probes | `probes/` | Per-pod probe agents that run a caller-supplied health check on a ticker, update the in-memory readiness map, and invoke an onUpdate callback so the async provider can push fresh status through v-k's notify hook |
| Metrics | `metrics/` | Prometheus collectors for pod lifecycle, snapshot pull / push, VM table size, orphans |
| Build metadata | `version/` | ldflags-injected version / revision / built-at strings |

## Lifecycle

### CreatePod

1. Parse `meta.VMSpec` from the pod annotations.
2. If a VM with `spec.VMName` already exists locally, adopt it (idempotent on restart).
3. Otherwise branch on `spec.Managed` first, then `spec.Mode`:
   - **`Managed=false`** (static / externally-managed VMs, e.g. Windows toolboxes on an external QEMU host): skip the runtime entirely and adopt the pre-assigned `VMID` / `IP` / `VNCPort` the operator pre-wrote into the `VMRuntime` annotations. `Managed` is the single source of truth for "vk-cocoon owns this VM's lifecycle".
   - **Mode `clone`** (default, `Managed=true`): look up the snapshot locally using a **tag-aware name** (`repo:tag`, or bare `repo` when the tag is `latest` for backward compatibility). If the local snapshot does not exist, pull it from epoch via `Puller.PullSnapshot`. Before cloning, `assertSnapshotBackend` validates the snapshot's recorded hypervisor matches `spec.Backend` — a CH snapshot cannot be cloned onto a FC target and vice-versa. When the snapshot carries a base image, `EnsureImage` pulls it (always attempts `cocoon image pull`; with `spec.ForcePull` it passes `--force` to bypass the URL-level cache). Then `Runtime.Clone(from=<local>, to=spec.VMName)`. For firecracker clones, `--cpu` and `--memory` overrides are stripped because FC cannot resize after snapshot/load.
   - **Mode `run`** (`Managed=true`): `EnsureImage` pulls the image (always attempts `cocoon image pull`; `--force` when `spec.ForcePull` is true), then `Runtime.Run(image=spec.Image, name=spec.VMName)`. When `spec.Backend` is `firecracker`, `--fc` is passed to select the FC backend; when `spec.OS` is `windows`, `--windows` is passed. When `spec.NoDirectIO` is true, `--no-direct-io` disables O_DIRECT on writable disks (CH only, useful for dev/test).
4. For clone/fork/wake paths, check whether the VM needs manual network setup (see [Post-clone hints](#post-clone-hints) below). If so, write the required commands as a base64-encoded annotation (`vm.cocoonstack.io/post-clone-hint`) and log a warning. The pod stays Running but Not Ready until the user executes the commands via `cocoon vm console` and the probe detects network connectivity.
5. Resolve the IP from the cocoon-net JSON lease file by MAC.
6. `meta.VMRuntime{VMID, IP}.Apply(pod)` writes the runtime annotations back so the operator and other consumers can pick them up. `VNCPort` is intentionally left unset here — cloud-hypervisor has no VNC server, so only the pre-seeded static-toolbox path ever carries a non-zero value.
7. Launch a per-pod probe agent in `probes/` (see [Readiness probing](#readiness-probing) below). The agent's first probe runs synchronously so the initial `notify` push already reflects reachability; later probes run on a ticker and call back into the provider whenever readiness flips so the async notify hook re-fires.

### DeletePod

1. Decode `meta.VMSpec`.
2. `meta.ShouldSnapshotVM(spec)` — the shared cocoon-common decoder — decides whether to snapshot before destroy:
   - `always`: `Runtime.SnapshotSave` then `Pusher.PushSnapshot(tag=meta.DefaultSnapshotTag)` to epoch.
   - `main-only`: same, but only when the VM name ends in `-0` (slot 0 = main agent).
   - `never`: skip snapshots entirely.
3. `Runtime.Remove(vmID)` to destroy the VM.
4. Forget the pod from the in-memory tables.

### UpdatePod

The only update vk-cocoon honors is a `HibernateState` transition. Anything else is a no-op (the operator deletes and recreates the pod for genuine spec changes).

| Transition | Behavior |
|---|---|
| `false → true` | `Runtime.SnapshotSave` → `Pusher.PushSnapshot(tag=meta.HibernateSnapshotTag)` → `Runtime.Remove`. Pod stays alive (`PodRunning`) so K8s controllers do not recreate it. **Compensating rollback**: if `Runtime.Remove` fails after a successful push, vk-cocoon best-effort `Registry.DeleteManifest` the hibernate tag so the operator does not observe `Hibernated` while the local VM is still running. Push and Save are idempotent, so a compensated retry re-publishes the tag cleanly on the next attempt. |
| `true → false` (with no live VM) | `Puller.PullSnapshot(tag=meta.HibernateSnapshotTag)` → `Runtime.Clone` → drop the hibernation tag from epoch. |

The operator's `CocoonHibernation` reconciler tracks the transition by polling `epoch.GetManifest(vmName, "hibernate")`.

### Node resources

On startup, vk-cocoon probes the host for real CPU, memory, hugepages, and disk capacity and registers them as the virtual node's `Capacity` and `Allocatable` in the Kubernetes API. This replaces the previous hardcoded defaults (128 CPU / 512Gi) with values the scheduler can trust.

- **Capacity** = raw host resources (`runtime.NumCPU`, `/proc/meminfo` MemTotal, `statfs` total, hugepages-2Mi)
- **Allocatable** = Capacity minus a reserve fraction (default 20%, override via `VK_RESERVE_PERCENT`)
- **Storage allocatable** uses `statfs` available bytes (`Bavail`) instead of total — base images, existing COW overlays, and snapshots are naturally excluded from the budget
- Values are read **once at startup** and do not update while vk-cocoon is running; a restart refreshes them (idempotent)
- Individual resources can be force-overridden via `VK_NODE_CPU`, `VK_NODE_MEM`, `VK_NODE_STORAGE`, `VK_NODE_HUGEPAGES`, `VK_NODE_PODS`

### Metrics and monitoring

vk-cocoon exposes three metrics surfaces:

**`:10250/stats/summary`** — kubelet stats API consumed by metrics-server and `kubectl top`. Reports per-pod CPU (cumulative nanoseconds from `/proc/<pid>/stat`) and memory (RSS from `/proc/<pid>/status`), plus per-pod network I/O from the TAP device inside each VM's network namespace (`/proc/<pid>/net/dev`). Node-level CPU and memory are read from `/proc/stat` and `/proc/meminfo`.

**`:10250/metrics/resource`** — Prometheus text format with the metric families metrics-server and HPA require: `node_cpu_usage_seconds_total`, `node_memory_working_set_bytes`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `pod_cpu_usage_seconds_total`, `pod_memory_working_set_bytes`.

**`:9091/metrics`** — Prometheus endpoint with vk-cocoon-specific metrics:

| Metric | Type | Description |
|---|---|---|
| `vk_cocoon_vm_cpu_seconds_total{vm,pod,namespace,backend}` | Counter | Per-VM cumulative CPU |
| `vk_cocoon_vm_memory_rss_bytes{vm,pod,namespace,backend}` | Gauge | Per-VM RSS |
| `vk_cocoon_vm_disk_cow_bytes{vm,pod,namespace,backend}` | Gauge | Per-VM COW overlay actual size |
| `vk_cocoon_vm_network_rx_bytes_total` / `tx_bytes_total` | Counter | Per-VM TAP network I/O |
| `vk_cocoon_node_cpu_seconds_total` | Counter | Node cumulative CPU |
| `vk_cocoon_node_memory_used_bytes` | Gauge | Node used memory |
| `vk_cocoon_node_storage_available_bytes` / `total_bytes` | Gauge | Cocoon root filesystem |
| `vk_cocoon_vm_boot_duration_seconds{mode,backend}` | Histogram | VM creation time (run or clone) |
| `vk_cocoon_snapshot_save_duration_seconds` | Histogram | Snapshot save time |
| `vk_cocoon_snapshot_push_duration_seconds` | Histogram | Epoch push time |
| `vk_cocoon_snapshot_pull_duration_seconds` | Histogram | Epoch pull time |
| `vk_cocoon_probe_duration_seconds` | Histogram | Per-probe ICMP ping time |
| `vk_cocoon_pod_lifecycle_total{op,result}` | Counter | Pod lifecycle operations |
| `vk_cocoon_snapshot_pull_total{result}` / `push_total` | Counter | Snapshot pull/push counts |
| `vk_cocoon_vm_table_size` | Gauge | Tracked VM count |
| `vk_cocoon_orphan_vm_total` | Counter | Orphan VMs at startup |

All per-VM stats are read from `/proc` using the hypervisor PID tracked in memory — no shell-out to `cocoon` on each scrape. The tracking table is snapshot-copied under RLock and `/proc` reads happen outside the lock to avoid blocking CreatePod/DeletePod. When a VM is restarted in-place (event watcher → `cocoon vm start`), the PID is re-inspected and refreshed.

### Post-clone hints

After a clone (or hibernate wake / fork), vk-cocoon checks whether the VM needs manual guest-side network setup. The only fully automatic case is CH + OCI + all-DHCP (NIC hot-swap triggers systemd-networkd to re-DHCP). All other combinations require intervention:

| Scenario | Reason | Hint commands |
|---|---|---|
| CH + cloudimg (any network) | snapshot restore does not re-trigger cloud-init | `cloud-init clean + init` |
| CH + OCI + static IP | guest retains old IP config | write MAC-based networkd files |
| FC (any) | guest MAC frozen in vmstate | `ip link set address` + networkd reconfig |

When a hint is needed, vk-cocoon base64-encodes the required shell commands into `vm.cocoonstack.io/post-clone-hint` on the pod and logs a warning. The pod stays Running but Not Ready. To retrieve the commands: `kubectl get pod <name> -o jsonpath='{.metadata.annotations.vm\.cocoonstack\.io/post-clone-hint}' | base64 -d`. After executing via `cocoon vm console`, the probe detects connectivity and flips Ready automatically.

Classification uses the snapshot's original image URL (normal clone) or the COW file type on disk (fork/wake) to distinguish cloudimg from OCI.

### Startup reconcile

Cluster state is the source of truth. There is **no** persistent `pods.json` file. On every restart vk-cocoon:

1. Lists every pod scheduled to its node via `fieldSelector=spec.nodeName=<VK_NODE_NAME>`.
2. Lists every VM the cocoon runtime knows about via `Runtime.List`.
3. Adopts each pod with a `vm.cocoonstack.io/id` annotation by matching the VMID against the runtime list.
4. Walks unmatched VMs through the configured `VK_ORPHAN_POLICY`:
   - `alert` (default): log + bump `vk_cocoon_orphan_vm_total`, leave the VM alone.
   - `destroy`: remove the VM.
   - `keep`: no log, no metric.

A pod whose annotated VMID does **not** appear in the local runtime list logs a warning and is left to `CreatePod` to recreate on the next reconcile.

### Readiness probing

vk-cocoon implements v-k's `NotifyPods` interface, so the framework treats it as an **async provider**: Kubernetes only sees the pod status vk-cocoon actively pushes through `notify`, and v-k never polls `GetPodStatus` on its own. That makes a real per-pod probe loop load-bearing — any status change that happens after `CreatePod` returns is invisible to the cluster unless vk-cocoon re-fires `notify`.

The `probes/` package owns that loop:

1. `CreatePod` (and startup reconcile) call `Manager.Start(key, probe, onUpdate)`. The probe closure the provider supplies performs three checks in order:
    1. The tracked VM still exists.
    2. If the in-memory VM record has no IP, re-try the cocoon-net lease file by MAC and write it back via `setVMIP`.
    3. `Pinger.Ping(ctx, ip)` — a single ICMPv4 echo. This matches the cocoon Windows golden image contract (`windows/autounattend.xml` explicitly opens `icmpv4:8` and disables all firewall profiles), and it decouples readiness from specific services so the same probe works for Linux and Windows guests alike.
2. The first probe runs **synchronously inside `Start`** so the refreshStatus/notify pass that `CreatePod` does before returning already reflects the initial reachability decision.
3. A background goroutine re-runs the probe on a ticker (2 s cold-start, 5 s once Ready) and invokes `onUpdate` after 3 consecutive failures flip readiness back to false. `onUpdate` re-reads the pod, rebuilds the status, and calls `notify` so the kubelet observes the change.
4. `DeletePod` calls `Manager.Forget`, which cancels the per-pod goroutine; `Manager.Close` is called once at shutdown to tear every remaining agent down.

### VM event watcher

In addition to the periodic probe, vk-cocoon subscribes to cocoon's real-time VM event stream via `cocoon vm status --event --format json`. This provides sub-second detection of VM state changes (DELETED, stopped, error) without waiting for the next probe tick.

The watcher goroutine (`vmWatchLoop`) runs for the lifetime of the process with automatic restart on subprocess failure (2 s backoff). When an event arrives:

| Event | Inspect result | Action |
|---|---|---|
| `DELETED` | VM not found | `evictPod`: delete pod from API server → operator recreates |
| `MODIFIED` (state ≠ running) | state = stopped/error | `cocoon vm start` (in-place restart, preserves disk/network) |
| `MODIFIED` (state ≠ running) | state = running | False alarm — ignore |

A 30-second **restart cooldown** (`restartCooldown`) prevents tight restart loops when a VM keeps crashing. If the cooldown has not elapsed since the last restart, the pod is evicted instead so the operator can do a clean recreation.

Detection latency comparison:

| Mechanism | Worst-case latency |
|---|---|
| Probe only (old: 15 s × 5 failures) | ~75 s |
| Probe only (current: 5 s × 3 failures) | ~24 s |
| VM event watcher | **< 1 s** |

If the ICMP raw socket cannot be opened — typically because the binary is running without `CAP_NET_RAW` — the provider falls back to `network.NopPinger` and the probe degrades to "an IP was resolved == Ready". That is weaker than a real end-to-end ping but still strictly better than the previous behaviour of marking the pod Ready the instant `cocoon vm clone/run` returned. The systemd unit in `packaging/vk-cocoon.service` grants `AmbientCapabilities=CAP_NET_RAW` so the production path gets the real pinger.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `KUBECONFIG` | unset | Path to kubeconfig (in-cluster used otherwise). |
| `VK_NODE_NAME` | `cocoon-pool` | Virtual node name registered with the K8s API. |
| `VK_LOG_LEVEL` | `info` | `projecteru2/core/log` level. |
| `EPOCH_URL` | `http://epoch.cocoon-system.svc:8080` | Epoch base URL. |
| `EPOCH_TOKEN` | unset | Bearer token (only needed for `/v2/` pushes; `/dl/` is anonymous). |
| `VK_LEASES_PATH` | `/var/lib/cocoon/net/leases.json` | cocoon-net JSON lease file. |
| `VK_COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary. |
| `VK_SSH_PASSWORD` | unset | SSH password for `kubectl logs / exec` against Linux guests. |
| `VK_ORPHAN_POLICY` | `alert` | `alert`, `destroy`, or `keep`. |
| `VK_NODE_IP` | auto-detected | Override the virtual node's InternalIP address (first non-loopback IPv4 used otherwise). |
| `VK_NODE_POOL` | `default` | Cocoon pool label stamped onto the registered node. |
| `VK_PROVIDER_ID` | unset | Cloud-provider ProviderID for the virtual node (e.g. `gce://<project>/<zone>/<instance>`). Prevents cloud node lifecycle controllers from deleting the virtual node. |
| `VK_TLS_CERT` | `/etc/cocoon/vk/tls/vk-kubelet.crt` | Path to the kubelet serving TLS certificate. |
| `VK_TLS_KEY` | `/etc/cocoon/vk/tls/vk-kubelet.key` | Path to the kubelet serving TLS private key. |
| `VK_METRICS_ADDR` | `:9091` | Plain-HTTP prometheus listener. |
| `VK_RESERVE_PERCENT` | `20` | Percentage of host resources reserved for the host OS (0-100). Allocatable = Capacity × (100 - reserve) / 100. |
| `VK_NODE_CPU` | auto-detected | Override CPU capacity (auto: `runtime.NumCPU()`). |
| `VK_NODE_MEM` | auto-detected | Override memory capacity (auto: `/proc/meminfo` MemTotal). |
| `VK_NODE_STORAGE` | auto-detected | Override storage capacity (auto: `statfs` on `COCOON_ROOT_DIR`). |
| `VK_NODE_HUGEPAGES` | auto-detected | Override hugepages-2Mi capacity (auto: `/proc/meminfo`). |
| `VK_NODE_PODS` | `256` | Maximum pod count. |

## Installation

vk-cocoon is a host-level binary, not a kubernetes Deployment. The recommended path is the supplied systemd unit:

```bash
sudo install -m 0755 ./vk-cocoon /usr/local/bin/vk-cocoon
sudo install -m 0644 packaging/vk-cocoon.service /etc/systemd/system/vk-cocoon.service
sudo install -m 0644 packaging/vk-cocoon.env.example /etc/cocoon/vk-cocoon.env

# edit /etc/cocoon/vk-cocoon.env to your environment

sudo systemctl daemon-reload
sudo systemctl enable --now vk-cocoon
```

The unit reads `/etc/cocoon/kubeconfig` for cluster credentials and `/etc/cocoon/vk-cocoon.env` for the variables above.

## Development

```bash
make all            # full pipeline: deps + fmt + lint + test + build
make build          # build vk-cocoon binary
make test           # vet + race-detected tests
make lint           # golangci-lint on linux + darwin
make fmt            # gofumpt + goimports
make help           # show all targets
```

The Makefile detects Go workspace mode (`go env GOWORK`) and skips `go mod tidy` when active so cross-module references resolve through `go.work` without forcing a release of cocoon-common or epoch.

## Related projects

| Project | Role |
|---|---|
| [cocoon](https://github.com/cocoonstack/cocoon) | The MicroVM runtime vk-cocoon shells out to. |
| [cocoon-common](https://github.com/cocoonstack/cocoon-common) | CRD types, annotation contract, shared helpers. |
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and CocoonHibernation reconcilers. |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Admission webhook for sticky scheduling and CocoonSet validation. |
| [epoch](https://github.com/cocoonstack/epoch) | Snapshot registry; vk-cocoon pulls and pushes via `epoch/snapshot` + `epoch/cloudimg`. |
| [cocoon-net](https://github.com/cocoonstack/cocoon-net) | Per-host networking with embedded DHCP server and iptables setup; vk-cocoon reads its JSON lease file. |

## License

[MIT](LICENSE)
