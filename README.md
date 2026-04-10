# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to [Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs.

## Overview

vk-cocoon is the host-side bridge between the Kubernetes API and the cocoon runtime running on a single node. It satisfies the [virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider contract by translating pod CRUD into cocoon CLI calls and reporting per-VM status back to the kubelet.

| Layer | Package | Responsibility |
|---|---|---|
| Application | `package main` | `CocoonProvider`, lifecycle methods (CreatePod / DeletePod / UpdatePod / GetPodStatus / GetContainerLogs / RunInContainer), startup reconcile, orphan policy |
| Cocoon CLI | `vm/` | `Runtime` interface + the default `CocoonCLI` implementation that shells out to `sudo cocoon …` |
| Snapshot SDK | `snapshots/` | Wraps the [epoch](https://github.com/cocoonstack/epoch) SDK as a `RegistryClient` interface, plus `Puller` and `Pusher` that stream snapshots and cloud images via `epoch/snapshot` and `epoch/cloudimg` |
| Network | `network/` | dnsmasq lease parser used to resolve a freshly cloned VM's IP |
| Guest exec | `guest/` | SSH executor (Linux) and RDP help-text shim (Windows) |
| Probes | `probes/` | In-memory readiness/liveness map for the GetPodStatus hot path |
| Metrics | `metrics/` | Prometheus collectors for pod lifecycle, snapshot pull / push, VM table size, orphans |
| Build metadata | `version/` | ldflags-injected version / revision / built-at strings |

## Lifecycle

### CreatePod

1. Parse `meta.VMSpec` from the pod annotations.
2. If a VM with `spec.VMName` already exists locally, adopt it (idempotent on restart).
3. Otherwise:
   - **Mode `clone`** (default): pull the snapshot from epoch via `Puller.PullSnapshot` if not cached locally, then `Runtime.Clone(from=spec.Image | spec.ForkFrom, to=spec.VMName)`.
   - **Mode `run`**: `Runtime.Run(image=spec.Image, name=spec.VMName)`.
   - **Mode `static`**: skip the runtime and adopt the pre-assigned `staticIP` / `staticVMID`.
4. Resolve the IP from the dnsmasq lease file by MAC.
5. `meta.VMRuntime{VMID, IP}.Apply(pod)` writes the runtime annotations back so the operator and other consumers can pick them up.

### DeletePod

1. Decode `meta.VMSpec`.
2. Apply the snapshot policy:
   - `always`: `Runtime.SnapshotSave` then `Pusher.PushSnapshot` to epoch.
   - `main-only`: same, but only when the VM name ends in `-0` (slot 0 = main agent).
   - `never`: skip snapshots entirely.
3. `Runtime.Remove(vmID)` to destroy the VM.
4. Forget the pod from the in-memory tables.

### UpdatePod

The only update vk-cocoon honors is a `HibernateState` transition. Anything else is a no-op (the operator deletes and recreates the pod for genuine spec changes).

| Transition | Behavior |
|---|---|
| `false → true` | `Runtime.SnapshotSave` → `Pusher.PushSnapshot(tag=meta.HibernateSnapshotTag)` → `Runtime.Remove`. Pod stays alive (`PodRunning`) so K8s controllers do not recreate it. |
| `true → false` (with no live VM) | `Puller.PullSnapshot(tag=meta.HibernateSnapshotTag)` → `Runtime.Clone` → drop the hibernation tag from epoch. |

The operator's `CocoonHibernation` reconciler tracks the transition by polling `epoch.GetManifest(vmName, "hibernate")`.

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

## Configuration

| Variable | Default | Description |
|---|---|---|
| `KUBECONFIG` | unset | Path to kubeconfig (in-cluster used otherwise). |
| `VK_NODE_NAME` | `cocoon-pool` | Virtual node name registered with the K8s API. |
| `VK_LOG_LEVEL` | `info` | `projecteru2/core/log` level. |
| `EPOCH_URL` | `http://epoch.cocoon-system.svc:8080` | Epoch base URL. |
| `EPOCH_TOKEN` | unset | Bearer token (only needed for `/v2/` pushes; `/dl/` is anonymous). |
| `VK_LEASES_PATH` | `/var/lib/dnsmasq/dnsmasq.leases` | dnsmasq lease file. |
| `VK_COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary. |
| `VK_SSH_PASSWORD` | unset | SSH password for `kubectl logs / exec` against Linux guests. |
| `VK_ORPHAN_POLICY` | `alert` | `alert`, `destroy`, or `keep`. |
| `VK_METRICS_ADDR` | `:9091` | Plain-HTTP prometheus listener. |

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
| [cocoon-net](https://github.com/cocoonstack/cocoon-net) | Per-host networking provisioning (dnsmasq + iptables); vk-cocoon reads its lease file. |

## License

[MIT](LICENSE)
