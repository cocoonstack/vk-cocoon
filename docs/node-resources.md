# Node resources

On startup, vk-cocoon probes the host for real CPU, memory, hugepages,
and disk capacity and registers them as the virtual node's `Capacity` and
`Allocatable` in the Kubernetes API. This replaces hardcoded defaults
with values the scheduler can trust.

- **Capacity** = raw host resources (`runtime.NumCPU`, `/proc/meminfo`
  MemTotal, `statfs` total, and `hugepages-<size>` where the page size is
  read from `/proc/meminfo` so a 1Gi-default node is advertised under the
  right key).
- **Allocatable** = Capacity minus a reserve fraction (default 20%,
  override via `VK_RESERVE_PERCENT`). The reserve is accounting only;
  pair it with cocoon's `cgroup_cpus` fence to make it physical — the
  fence keeps VM threads (vCPU, virtio, io_uring workers) off the
  reserved cores, which then serve vk-cocoon's own probe loops,
  clone/wake execution, and snapshot transfers.
- **Storage allocatable** uses `statfs` available bytes (`Bavail`)
  instead of total — base images, existing COW overlays, and snapshots
  are naturally excluded from the budget.
- Values are read **once at startup** and do not update while vk-cocoon
  is running; a restart refreshes them (idempotent).
- Individual resources can be force-overridden via `VK_NODE_CPU`,
  `VK_NODE_MEM`, `VK_NODE_STORAGE`, `VK_NODE_HUGEPAGES`, `VK_NODE_PODS`
  (see [Configuration](configuration.md)).
