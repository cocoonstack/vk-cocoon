# vk-cocoon

Virtual Kubelet provider that maps Kubernetes pods to [Cocoon](https://github.com/cocoonstack/cocoon) MicroVMs.

## Overview

`vk-cocoon` registers a virtual node in Kubernetes and translates pod lifecycle operations into VM operations. Each pod becomes a full MicroVM managed by the Cocoon runtime and Cloud Hypervisor.

- **Snapshot-aware lifecycle** -- create from snapshots, hibernate running VMs to Epoch, and restore on wake
- **Slot-based naming** -- Deployment replicas get stable VM names with deterministic slot allocation
- **Live fork** -- sub-agents clone from the main agent (slot 0) via live snapshots
- **Full kubectl support** -- `exec`, `logs`, `attach`, and `port-forward` bridged through SSH
- **Liveness and readiness probes** -- exec, TCP, and HTTP probes run against the guest VM
- **ConfigMap and Secret injection** -- volumes and env vars written into the VM via SSH after boot
- **Windows guests** -- first-class support with RDP access on port 3389
- **Real metrics** -- CPU and memory stats from Cloud Hypervisor API and host `/proc`

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
| clone | `cocoon.cis/mode=clone` | Cold-clone from a local or remote snapshot (default) |
| run | `cocoon.cis/mode=run` | Boot from a cloud image |
| adopt | `cocoon.cis/mode=adopt` | Attach to an existing Cocoon VM |
| static | `cocoon.cis/mode=static` | Track an externally managed VM by IP |

### Common annotations

| Annotation | Purpose |
|---|---|
| `cocoon.cis/image` | Snapshot name or Epoch URL |
| `cocoon.cis/os` | Guest OS: `linux` (default) or `windows` |
| `cocoon.cis/storage` | COW disk size, e.g. `100G` |
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
- (Optional) An [Epoch](https://github.com/cocoonstack/epoch) registry for snapshot storage

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
| `KUBECONFIG` | `~/.kube/config` | Path to kubeconfig |
| `VK_NODE_NAME` | `cocoon-pool` | Virtual node name in Kubernetes |
| `VK_NODE_IP` | auto-detected | IP address of the host running vk-cocoon |
| `VK_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary |
| `COCOON_SSH_PASSWORD` | (none) | Default SSH password for VM access |
| `EPOCH_REGISTRY_TOKEN` | (none) | Bearer token for Epoch registry auth |
| `VK_TLS_CERT` / `VK_TLS_KEY` | self-signed | TLS cert/key for the kubelet API |

## Quick Start

```bash
export KUBECONFIG=$HOME/.kube/config
export VK_NODE_NAME=cocoon-pool
export COCOON_BIN=/usr/local/bin/cocoon

./vk-cocoon
```

See [DEPLOY.md](DEPLOY.md) for worker prerequisites and environment variables.

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

See [DESIGN.md](DESIGN.md) for the provider's design rationale covering restart recovery, networking, hibernation, and Windows support.

## Related Projects

| Project | Role |
|---|---|
| [cocoon-operator](https://github.com/cocoonstack/cocoon-operator) | CocoonSet and Hibernation controllers |
| [epoch](https://github.com/cocoonstack/epoch) | Snapshot registry |
| [cocoon-webhook](https://github.com/cocoonstack/cocoon-webhook) | Sticky scheduling webhook |
| [glance](https://github.com/cocoonstack/glance) | Browser-based SSH, RDP, and VNC access |

## License

[MIT](LICENSE)
