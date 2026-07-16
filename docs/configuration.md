# Configuration

vk-cocoon is configured entirely through environment variables. The
systemd unit reads them from `/etc/cocoon/vk-cocoon.env`.

| Variable | Default | Description |
|---|---|---|
| `KUBECONFIG` | unset | Path to kubeconfig (in-cluster used otherwise). |
| `VK_NODE_NAME` | `cocoon-pool` | Virtual node name registered with the K8s API. |
| `VK_LOG_LEVEL` | `info` | `projecteru2/core/log` level. |
| `OCI_REGISTRY` | **required** | OCI registry base for snapshots and cloud images (e.g. an Artifact Registry repo). Auth resolves GCP ADC then docker config. |
| `GOOGLE_APPLICATION_CREDENTIALS` | unset | Path to a GCP service-account JSON key with `roles/artifactregistry.writer`, fed to ADC for the snapshot push. Unset falls back to the read-only node instance SA. |
| `VK_LEASES_PATH` | `/var/lib/cocoon/net/leases.json` | cocoon-net JSON lease file. |
| `VK_COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary. |
| `VK_ORPHAN_POLICY` | `destroy` | `destroy` (auto-clean), `alert`, or `keep`. |
| `VK_RESTORE_MODE` | `ondemand` | Guest-memory restore mode for Cloud Hypervisor clones: `copy`, `ondemand`, or `mmap`. Windows VMs always use `copy` (lazy restore stalls DHCP boot); Firecracker has no restore mode. `mmap` shares page cache across clones of one snapshot — the fastest fan-out — but requires a Cloud Hypervisor build with mmap restore support (cocoonstack/cloud-hypervisor `dev`); on other CH builds clones fail, so it is opt-in. Invalid values abort startup. |
| `VK_NODE_IP` | auto-detected | Override the virtual node's InternalIP address (first non-loopback IPv4 used otherwise). |
| `VK_NODE_POOL` | `default` | Cocoon pool label stamped onto the registered node. |
| `VK_PROVIDER_ID` | unset | Cloud-provider ProviderID for the virtual node (e.g. `gce://<project>/<zone>/<instance>`). Prevents cloud node lifecycle controllers from deleting the virtual node. |
| `VK_TLS_CERT` | `/etc/cocoon/vk/tls/vk-kubelet.crt` | Path to the kubelet serving TLS certificate. |
| `VK_TLS_KEY` | `/etc/cocoon/vk/tls/vk-kubelet.key` | Path to the kubelet serving TLS private key. |
| `VK_KUBELET_PORT` | `10250` | Port the virtual node's kubelet API listens on and advertises. Override when a real kubelet on the same host already owns `:10250` (e.g. a co-located k3s server node). |
| `VK_METRICS_ADDR` | `:9091` | Plain-HTTP prometheus listener. |
| `VK_RESERVE_PERCENT` | `20` | Percentage of host resources reserved for the host OS (0-100). Allocatable = Capacity × (100 - reserve) / 100. |
| `VK_NODE_CPU` | auto-detected | Override CPU capacity (auto: `runtime.NumCPU()`). |
| `VK_NODE_MEM` | auto-detected | Override memory capacity (auto: `/proc/meminfo` MemTotal). |
| `VK_NODE_STORAGE` | auto-detected | Override storage capacity (auto: `statfs` on `COCOON_ROOT_DIR`). |
| `VK_NODE_HUGEPAGES` | auto-detected | Override hugepages capacity (auto: `/proc/meminfo`; keyed by the host's default page size). |
| `VK_NODE_PODS` | `256` | Maximum pod count. |
