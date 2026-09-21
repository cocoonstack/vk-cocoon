# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to
[Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs. One vk-cocoon process
runs per node and satisfies the
[virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet)
provider contract by translating pod CRUD into `cocoon` CLI calls and
pushing per-VM status back to the kubelet.

**Documentation: [cocoonstack.github.io/vk-cocoon](https://cocoonstack.github.io/vk-cocoon/)**

## Architecture

```
Kubernetes API ──► virtual-kubelet provider (vk-cocoon, one per node)
   pod CRUD    ──► CreatePod / DeletePod / UpdatePod ── cocoon clone/run/snapshot
   status      ◄── async notify ── per-pod probe loop + real-time VM event watcher
   snapshots   ──► Puller / Pusher ── OCI registry (cross-node hibernate/wake)
```

| Layer | Package | Responsibility |
|---|---|---|
| Application | `package main` | Entry point, node registration, metrics server, VM event watcher startup |
| Provider | `provider/cocoon/` | Lifecycle methods, startup reconcile, orphan policy, VM event watcher, pod eviction |
| Provider types | `provider/` | Shared orphan policy, `VMStats` / `NodeStats`, and node-capacity helpers |
| Cocoon CLI | `vm/` | `Runtime` interface + the `CocoonCLI` that shells out to `cocoon` |
| Snapshot SDK | `snapshots/` | `Puller` / `Pusher` stream snapshots and cloud images to an OCI registry |
| Network | `network/` | cocoon-net lease parser, the lease-release control-socket client, and the ICMPv4 `Pinger` the probe loop uses |
| Guest console | `guest/` | SAC dialer for Windows static IP |
| Probes | `probes/` | Per-pod probe agents that keep the async provider's pushed status live |
| Metrics | `metrics/` | Prometheus collectors for lifecycle, snapshots, VM table, orphans |

See [Architecture](docs/architecture.md) for the full layer map and the
async-provider contract.

### macOS guests

Managed macOS guests use the standalone cocoon-macos backend; see
[macOS lifecycle and networking](docs/lifecycle.md#macos-guests).

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

## Related projects

| Project | Role |
|---|---|
| [cocoon](https://github.com/cocoonstack/cocoon) | The MicroVM runtime vk-cocoon shells out to |
| [cocoon-common](https://github.com/cocoonstack/cocoon-common) | CRD types, annotation contract, OCI registry + snapshot/cloud-image packages |
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and CocoonHibernation reconcilers |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Admission webhook for sticky scheduling and CocoonSet validation |
| [cocoon-net](https://github.com/cocoonstack/cocoon-net) | Per-host networking; vk-cocoon reads its JSON lease file and releases leases over its control socket (≥ v0.2.2) |

## Development

```bash
make all     # deps + fmt + lint + test + build
make build   # build the vk-cocoon binary
make test    # vet + race-detected tests
make lint    # golangci-lint on linux + darwin
make fmt     # gofumpt + goimports
make help    # show all targets
```

## License

[MIT](LICENSE)
