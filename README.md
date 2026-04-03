# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to [Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs.

## Overview

- **Snapshot-aware lifecycle** -- create from snapshots, hibernate running VMs to Epoch, and restore on wake
- **Restart recovery** -- persistent pod state with event-driven VM cache; adopt-on-restart reconnects to surviving VMs
- **Slot-based naming** -- Deployment replicas get stable VM names with deterministic slot allocation
- **Live fork** -- sub-agents clone from the main agent (slot 0) via live snapshots
- **Streaming snapshot I/O** -- snapshot import and export via `cocoon snapshot import/export` CLI for reliable archive handling
- **Full kubectl support** -- `exec`, `logs`, `attach`, and `port-forward` bridged through SSH
- **Liveness and readiness probes** -- exec, TCP, and HTTP probes run against the guest VM
- **ConfigMap and Secret injection** -- volumes and env vars written into the VM via SSH after boot
- **Windows guests** -- first-class support with RDP access on port 3389
- **Real metrics** -- CPU and memory stats from the Cloud Hypervisor API and host `/proc`

Each pod becomes a full MicroVM managed by the Cocoon runtime and Cloud Hypervisor.

## Architecture

```text
Kubernetes API
      |
      v
  vk-cocoon  --->  Epoch (snapshot registry)
      |
      v
  Cocoon runtime  --->  Cloud Hypervisor  --->  Guest VM
```

### Pod modes

| Mode | Annotation | Behavior |
|---|---|---|
| clone | `cocoon.cis/mode=clone` | Ensure a snapshot exists locally, then clone the VM from that snapshot (default) |
| run | `cocoon.cis/mode=run` | Boot from a cloud image |
| adopt | `cocoon.cis/mode=adopt` | Attach to an existing Cocoon VM |
| static | `cocoon.cis/mode=static` | Track an externally managed VM by IP |

### Common annotations

| Annotation | Purpose |
|---|---|
| `cocoon.cis/image` | Snapshot name or Epoch URL |
| `cocoon.cis/os` | Guest OS: `linux` (default) or `windows` |
| `cocoon.cis/storage` | COW disk size, e.g. `100G` (defaults: Linux `100G`, Windows `15G`) |
| `cocoon.cis/hibernate` | Set to `true` to hibernate the VM |
| `cocoon.cis/vm-name` | Stable VM name for restart recovery |
| `cocoon.cis/fork-from` | Source VM name for live-fork |
| `cocoon.cis/snapshot-policy` | `always`, `main-only`, or `never` |

## Installation

### Prerequisites

- Go 1.25 or later
- A running Kubernetes cluster with `kubectl` configured
- [Cocoon](https://github.com/cocoonstack/cocoon) runtime installed on the target node
- `sshpass` for SSH-based VM access
- Optional [Epoch](https://github.com/cocoonstack/epoch) registry for snapshot storage

### Download

Download a pre-built binary from [GitHub Releases](https://github.com/cocoonstack/vk-cocoon/releases).

### Build from source

```bash
git clone https://github.com/cocoonstack/vk-cocoon.git
cd vk-cocoon
make build          # produces ./vk-cocoon
sudo mv vk-cocoon /usr/local/bin/
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `KUBECONFIG` | `/etc/cocoon/kubeconfig` when present, otherwise standard fallback | Path to kubeconfig |
| `VK_NODE_NAME` | `cocoon-pool` | Virtual node name in Kubernetes |
| `VK_NODE_IP` | auto-detected | IP address of the host running vk-cocoon |
| `VK_LOG_LEVEL` | `info` | Log level for the provider process |
| `COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary |
| `COCOON_SSH_PASSWORD` | (none) | Default SSH password for VM access |
| `EPOCH_REGISTRY_TOKEN` | (none) | Bearer token for Epoch registry auth |
| `VK_TLS_CERT` / `VK_TLS_KEY` | self-signed | TLS cert and key for the kubelet API |

## Quick Start

```bash
sudo install -d -m 0755 /etc/cocoon
sudo cp /path/to/kubeconfig /etc/cocoon/kubeconfig

export KUBECONFIG=/etc/cocoon/kubeconfig
export VK_NODE_NAME=cocoon-pool
export COCOON_BIN=/usr/local/bin/cocoon

./vk-cocoon
```

### Run with systemd

```bash
sudo install -m 0755 vk-cocoon /usr/local/bin/vk-cocoon
sudo install -D -m 0644 vk-cocoon.service /etc/systemd/system/vk-cocoon.service
sudo install -d -m 0755 /etc/cocoon
sudo cp /path/to/kubeconfig /etc/cocoon/kubeconfig
sudo systemctl daemon-reload
sudo systemctl enable --now vk-cocoon
```

The bundled unit starts `/usr/local/bin/vk-cocoon` and reads kubeconfig from `/etc/cocoon/kubeconfig`.

Optional overrides can go in `/etc/cocoon/vk-cocoon.env`, for example:

```bash
VK_NODE_NAME=cocoon-pool
VK_NODE_IP=192.0.2.10
COCOON_BIN=/usr/local/bin/cocoon
VK_LOG_LEVEL=info
```

Windows pods default to `2` vCPU, `4Gi` memory, and `15G` storage when limits or `cocoon.cis/storage` are not set.

## Usage

### Examples

- [manifests/test-ubuntu.yaml](manifests/test-ubuntu.yaml) -- Linux VM pod
- [manifests/windows-vm.yaml](manifests/windows-vm.yaml) -- Windows VM pod
- [manifests/test-cocoonset.yaml](manifests/test-cocoonset.yaml) -- CocoonSet with fork

## Development

```bash
make build          # build binary
make test           # run tests with race detection
make lint           # run golangci-lint
make fmt            # format code
make help           # show all targets
```

See [Design](https://github.com/cocoonstack/.github/blob/main/docs/vk-cocoon/design.md) for the provider's design rationale covering restart recovery, networking, hibernation, and Windows support.

## Related Projects

| Project | Role |
|---|---|
| [cocoon-common](https://github.com/cocoonstack/cocoon-common) | Shared metadata, Kubernetes, and logging helpers |
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and Hibernation controllers |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Sticky scheduling webhook |
| [epoch](https://github.com/cocoonstack/epoch) | Snapshot registry |

## License

[MIT](LICENSE)
