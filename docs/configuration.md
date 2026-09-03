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
| `VK_COCOON_NET_CONTROL_SOCKET` | `/run/cocoon-net/control.sock` | Root-only cocoon-net Unix socket used to reclaim DHCP leases after VM destruction. Needs cocoon-net ≥ v0.2.2 on the node; older daemons have no control socket and every release falls back to lease expiry. |
| `VK_COCOON_BIN` | `/usr/local/bin/cocoon` | Path to the cocoon CLI binary. |
| `VK_COCOON_MACOS_BIN` | `/usr/local/bin/cocoon-macos` | cocoon-macos binary `os=macos` pods dispatch to. Needs cocoon-macos ≥ v0.1.8 (`--net cni`, `--storage`, `--exit-on-reboot`, `--random-smbios`); `--storage` must not shrink below the image's virtual size, and cocoon-macos requires an even vCPU count. Guests default to 4 vCPU and 8192 MiB when the pod declares no resources; `--memory` uses the pod's memory request, falling back to the limit. |
| `COCOON_MACOS_VNC_PASSWORD` | unset = VNC off | QEMU VNC password: at most 8 bytes, no control characters, validated at startup. When set, each macOS guest's VNC port (5900-5999) listens on **all node interfaces** — firewall the range. Unset leaves macOS guests without VNC. Never persisted in the VM record. |
| `VK_ORPHAN_POLICY` | `destroy` | `destroy` (auto-clean), `alert`, or `keep`. |
| `VK_RESTORE_MODE` | `mmap` | Guest-memory restore mode for Cloud Hypervisor clones: `copy`, `ondemand`, or `mmap`. Windows VMs always use `copy` (lazy restore stalls DHCP boot); Firecracker has no restore mode. `mmap` shares page cache across clones of one snapshot — the fastest fan-out — and is the default; it requires a Cloud Hypervisor build with mmap restore support (cocoonstack/cloud-hypervisor `dev`) — on other CH builds clones fail, so set `copy` there. Invalid values abort startup. |
| `VK_STAGING_DIR` | `/var/lib/cocoon/vk-staging` | Temporary destination for peer-restored raw snapshot files. Keep it on the Cocoon run-directory filesystem to preserve hardlinks; stale entries are swept at startup. |
| `VK_PEER_ADDR` | `:12501` | Plain-HTTP peer-snapshot listener, serving `GET /v1/snapshot-plan` and `GET /v1/snapshot-slice`. Peers dial the node's InternalIP on this port, so expose it only inside the trusted node network. |
| `VK_COCOON_SNAPSHOT_DIR` | `/var/lib/cocoon/snapshot/localfile` | Cocoon localfile snapshot store served to peers. Override it when Cocoon uses a different root directory. |
| `VK_NODE_IP` | auto-detected | Override the virtual node's InternalIP address (first non-loopback IPv4 used otherwise). |
| `VK_NODE_POOL` | `default` | Cocoon pool label stamped onto the registered node. |
| `VK_SNAPSHOT_CPU_CLASS` | unset | Guest-visible CPU ABI class stamped as `cocoonstack.io/snapshot-cpu-class`. Classified CocoonSets schedule only to an exact match; use a certified class such as `n2-cascade-lake-v1`, not a broad architecture such as `amd64`. |
| `VK_PROVIDER_ID` | unset | Cloud-provider ProviderID for the virtual node (e.g. `gce://<project>/<zone>/<instance>`). Prevents cloud node lifecycle controllers from deleting the virtual node. |
| `VK_TLS_CERT` | `/etc/cocoon/vk/tls/vk-kubelet.crt` | Path to the kubelet serving TLS certificate. |
| `VK_TLS_KEY` | `/etc/cocoon/vk/tls/vk-kubelet.key` | Path to the kubelet serving TLS private key. |
| `VK_KUBELET_PORT` | `10250` | Port the virtual node's kubelet API listens on and advertises. Override when a real kubelet on the same host already owns `:10250` (e.g. a co-located k3s server node). |
| `VK_METRICS_ADDR` | `:9091` | Plain-HTTP prometheus listener; also serves `/debug/pprof/` and `/debug/pprof/profile`. |
| `VK_RESERVE_PERCENT` | `20` | Percentage of host resources reserved for the host OS (0-100). Allocatable = Capacity × (100 - reserve) / 100. Align with cocoon's `cgroup_cpus` fence so the accounting reserve matches the physically fenced cores. |
| `COCOON_CGROUP_PARENT` | `cocoon.slice` | cgroup v2 slice holding cocoon's per-VM CPU scopes; must match cocoon's `cgroup_parent` config. Per-VM CPU usage and throttling stats are read from `<slice>/vm-<id>.scope/cpu.stat`. |
| `VK_NODE_CPU` | auto-detected | Override CPU capacity (auto: `runtime.NumCPU()`). |
| `VK_NODE_MEM` | auto-detected | Override memory capacity (auto: `/proc/meminfo` MemTotal). |
| `VK_NODE_STORAGE` | auto-detected | Override storage capacity (auto: `statfs` on `COCOON_ROOT_DIR`). |
| `COCOON_ROOT_DIR` | `/var/lib/cocoon` | Cocoon's data directory. Governs storage-capacity `statfs`, per-VM COW-size metrics, the SAC console socket path, and the cloudimg-overlay probe. |
| `VK_NODE_HUGEPAGES` | auto-detected | Override hugepages capacity (auto: `/proc/meminfo`; keyed by the host's default page size). |
| `VK_NODE_PODS` | `256` | Maximum pod count. |
| `SNAPSHOT_ZSTD_LEVEL` | `0` | Snapshot writer zstd level; `0` keeps compression disabled. |
| `SNAPSHOT_CHUNK_SIZE_MIB` | `0` | Snapshot writer chunk size; `0` keeps chunking disabled. |
| `SNAPSHOT_TRANSFER_CONCURRENCY` | `8` | Parallel chunk transfers for encoded snapshots. |
| `SNAPSHOT_MEMORY_BUDGET_MIB` | `9216` | Push pipeline buffer cap per active push. |
| `SNAPSHOT_PULL_BUDGET_MIB` | `2048` | Pull prefetch buffer cap per active pull. |

Keep zstd and chunking disabled until every fleet reader and snapshot tool
supports the v2 snapshot format. The push and pull memory budgets can be tuned
independently without changing the writer format.
