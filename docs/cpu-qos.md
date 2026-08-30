# CPU QoS

Pod CPU resources translate into cocoon's per-VM cgroup v2 policy, with
Kubernetes semantics: **limits bound, requests weigh**. The mapping runs
on `vm run` and on every clone path alike (wake included) — cocoon never
inherits cgroup knobs from a snapshot.

| Pod field | VM effect | cocoon flag |
|---|---|---|
| `limits.cpu` (rounded up) | vCPU count — guest parallelism and the hard cap | `--cpu` (run only; clones keep snapshot topology) |
| `limits.cpu` | quota: long-run average ceiling, kernel floor 1ms | `--cpu-quota-us` + `--cpu-period-us` |
| `requests.cpu` | contention share via kubelet's cgroup v2 shares→weight conversion, minimum 1; falls back to `limits.cpu` when unset | `--cpu-weight` |
| unset limit | no quota flag — cocoon's Guaranteed-at-N default (cap = vCPU count) | — |

Resources are declared on the CocoonSet (`spec.agent.resources`,
`spec.toolboxes[].resources`) and copied verbatim onto the pod; a plain
pod with the vk annotations works the same way.

## Recipes

```yaml
# Guaranteed — production agents, predictable performance:
# 2 vCPU, hard 2-core cap, weight 79. K8s Guaranteed semantics.
resources:
  requests: { cpu: "2", memory: 4Gi }
  limits:   { cpu: "2", memory: 4Gi }

# Burstable overcommit — density-first fleets:
# 4 vCPU, may burst to 4 cores when the node is idle, shrinks by
# weight (20 vs a Guaranteed neighbor's 79) under contention.
# The scheduler bin-packs on requests (500m).
resources:
  requests: { cpu: 500m, memory: 2Gi }
  limits:   { cpu: "4",  memory: 4Gi }

# Fractional cap — metering / low-value background agents:
# 1 vCPU capped at a true half core (cpu.max 50000/100000).
resources:
  limits: { cpu: 500m, memory: 1Gi }

# BestEffort — opportunistic work:
# weight 1 (kubelet's minimum share), yields to everything.
resources: {}
```

## Operational notes

- vCPU count rounds the limit up: `limits: 1500m` boots a 2-CPU guest
  capped at 1.5 cores — in-guest `nproc` no longer equals usable cores.
- Changing resources replaces the pod and rebuilds the VM (the operator
  reconciles resources by delete + recreate); in-VM state is lost, so
  schedule resource changes to land in a hibernate window.
- Throttling is observable on every surface:
  `container_cpu_cfs_throttled_{seconds,periods}_total` on
  `/metrics/resource`, `cocoon_vk_vm_cpu_throttled_{seconds,periods}_total`
  on `:9091`, and `cocoon vm list`'s THROTTLED column on the node. A VM
  with fast-growing throttled time is pinned at its quota — raise its
  limit or move it.
- Pair with the node-side fence: cocoon's `cgroup_cpus` keeps VM threads
  off reserved cores and `VK_RESERVE_PERCENT` keeps the accounting
  consistent (see [Node resources](node-resources.md)).
