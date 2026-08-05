# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to
[Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs.

One vk-cocoon process runs per node. It satisfies the
[virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet)
provider contract by translating pod CRUD into `cocoon` CLI calls and
pushing per-VM status back to the kubelet.

**Documentation: [cocoonstack.github.io/vk-cocoon](https://cocoonstack.github.io/vk-cocoon/)** (source in [`docs/`](docs/)).

```
Kubernetes API ──► virtual-kubelet provider (vk-cocoon, one per node)
   pod CRUD    ──► CreatePod / DeletePod / UpdatePod ── cocoon clone/run/snapshot
   status      ◄── async notify ── per-pod probe loop + real-time VM event watcher
   snapshots   ──► Puller / Pusher ── OCI registry (cross-node hibernate/wake)
```

## Architecture

| Layer | Package | Responsibility |
|---|---|---|
| Application | `package main` | Entry point, node registration, metrics server, VM event watcher startup |
| Provider | `provider/cocoon/` | Lifecycle methods, startup reconcile, orphan policy, VM event watcher, pod eviction |
| Provider iface | `provider/` | Shared provider interface and node-capacity helpers |
| Cocoon CLI | `vm/` | `Runtime` interface + the `CocoonCLI` that shells out to `cocoon` |
| Snapshot SDK | `snapshots/` | `Puller` / `Pusher` stream snapshots and cloud images to an OCI registry |
| Network | `network/` | cocoon-net lease parser + the ICMPv4 `Pinger` the probe loop uses |
| Guest console | `guest/` | SAC dialer for Windows static IP |
| Probes | `probes/` | Per-pod probe agents that keep the async provider's pushed status live |
| Metrics | `metrics/` | Prometheus collectors for lifecycle, snapshots, VM table, orphans |

See [Architecture](docs/architecture.md) for the full layer map and the
async-provider contract.

## Quick start

vk-cocoon is a host-level binary installed via a systemd unit:

```bash
sudo install -m 0755 ./vk-cocoon /usr/local/bin/vk-cocoon
sudo install -m 0644 packaging/vk-cocoon.service /etc/systemd/system/vk-cocoon.service
sudo install -m 0644 packaging/vk-cocoon.env.example /etc/cocoon/vk-cocoon.env
# edit /etc/cocoon/vk-cocoon.env, then:
sudo systemctl daemon-reload && sudo systemctl enable --now vk-cocoon
```

Full steps in [Installation](docs/installation.md).

## Documentation

- [Architecture](docs/architecture.md) — layer map, async-provider contract
- [Pod lifecycle](docs/lifecycle.md) — CreatePod / DeletePod / hibernate
- [Readiness probing](docs/probes.md) — the per-pod probe loop
- [Runtime reconciliation](docs/reconcile.md) — startup reconcile, VM events
- [Post-clone network hints](docs/post-clone.md) — manual guest fixups
- [Node resources](docs/node-resources.md) — host-probed Capacity/Allocatable
- [CPU QoS](docs/cpu-qos.md) — pod requests/limits onto per-VM cgroup policy
- [Metrics & monitoring](docs/metrics.md) — the three metrics surfaces
- [Configuration](docs/configuration.md) — every environment variable
- [Installation](docs/installation.md) — systemd unit and building from source

## Development

```bash
make all     # deps + fmt + lint + test + build
make build   # build the vk-cocoon binary
make test    # vet + race-detected tests
make lint    # golangci-lint on linux + darwin
make help    # show all targets
```

## Related projects

| Project | Role |
|---|---|
| [cocoon](https://github.com/cocoonstack/cocoon) | The MicroVM runtime vk-cocoon shells out to |
| [cocoon-common](https://github.com/cocoonstack/cocoon-common) | CRD types, annotation contract, OCI registry + snapshot/cloud-image packages |
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and CocoonHibernation reconcilers |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Admission webhook for sticky scheduling and CocoonSet validation |
| [cocoon-net](https://github.com/cocoonstack/cocoon-net) | Per-host networking; vk-cocoon reads its JSON lease file |

## License

[MIT](LICENSE)
