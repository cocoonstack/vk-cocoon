# Node resources

On startup, vk-cocoon probes the host for real CPU, memory, hugepages,
and disk capacity and registers them as the virtual node's `Capacity` and
`Allocatable` in the Kubernetes API. This replaces hardcoded defaults
with values the scheduler can trust.

- **Capacity** = raw host resources (`runtime.NumCPU`, `/proc/meminfo`
  MemTotal, `statfs` total, and `hugepages-<size>` where the page size is
  read from `/proc/meminfo` so a 1Gi-default node is advertised under the
  right key; a `VK_NODE_HUGEPAGES` override advertises under the host's
  page size too, falling back to `hugepages-2Mi` when `/proc/meminfo` has
  no `Hugepagesize`).
- **Allocatable** = Capacity minus a reserve fraction (default 20%,
  override via `VK_RESERVE_PERCENT`), applied to every resource except
  `pods`, which is passed through unreduced. The result is computed from
  the exact capacity and rounded down to a whole unit: whole cores for CPU
  (32 cores allocate 25, a `3100m` override allocates 2) and whole bytes
  otherwise. The reserve is accounting only;
  pair it with cocoon's `cgroup_cpus` fence to make it physical — the
  fence keeps VM threads (vCPU, virtio, io_uring workers) off the
  reserved cores, which then serve vk-cocoon's own probe loops,
  clone/wake execution, and snapshot transfers.
- **Storage allocatable** is `statfs` available bytes (`Bavail`) plus
  the bytes the tracked VMs' COW overlays already occupy (allocated
  blocks, not apparent size), minus the reserve. The scheduler subtracts
  every pod's ephemeral-storage request from allocatable, so adding the
  overlays back keeps a running VM from being counted twice, once in the
  lower `Bavail` and once in its request; base images, snapshots and
  anything else on the filesystem stay excluded. vk-cocoon recomputes it
  every minute and pushes the node status when it changes, so pulls,
  snapshot caches and VM churn reach the scheduler within a minute.
  `VK_NODE_STORAGE` overrides total and available alike and pins the
  value; the reserve fraction is then all that separates them.
- CPU, memory, hugepages and the pod count are read **once at startup**;
  a restart refreshes them (idempotent).
- Individual resources can be force-overridden via `VK_NODE_CPU`,
  `VK_NODE_MEM`, `VK_NODE_STORAGE`, `VK_NODE_HUGEPAGES`, `VK_NODE_PODS`
  (see [Configuration](configuration.md)).

## Node labels

vk-cocoon stamps the virtual node with:

| Label | Source | Meaning |
|---|---|---|
| `cocoonstack.io/pool` | `VK_NODE_POOL` (default `default`) | The cocoon node pool CocoonSets select with `spec.nodePool` |
| `cocoonstack.io/snapshot-cpu-class` | `VK_SNAPSHOT_CPU_CLASS` (unset = absent) | The guest-visible CPU ABI this node can resume memory snapshots for; see [Snapshot CPU compatibility](lifecycle.md#snapshot-cpu-compatibility) |
| `node-role.kubernetes.io/cocoon-vm` | always | Marks the node as VM-backed |

The labels are re-asserted on every process start, on the node template
at registration and again on the live node object afterwards —
virtual-kubelet only patches the status subresource once the node exists,
so a re-stamped or cleared class would otherwise never reach an existing
node. Editing the env and restarting is therefore the supported way to
reclassify a node.
