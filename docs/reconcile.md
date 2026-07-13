# Runtime reconciliation

Cluster state is the source of truth. vk-cocoon keeps **no** persistent
`pods.json` — it rebuilds its in-memory tables from the Kubernetes API on
every restart, and it reacts to cocoon's live event stream in between.

## Startup reconcile

On every restart vk-cocoon:

1. Lists every pod scheduled to its node via
   `fieldSelector=spec.nodeName=<VK_NODE_NAME>`.
2. Lists every VM the cocoon runtime knows about via `Runtime.List`.
3. Adopts each pod with a `vm.cocoonstack.io/id` annotation by matching
   the VMID against the runtime list.
4. Walks unmatched VMs through the configured `VK_ORPHAN_POLICY`:
   - `destroy` (default): remove the VM so pod-less VMs don't accumulate
     after restart or pod chaos.
   - `alert`: log + bump `cocoon_vk_orphan_vm_total`, leave the VM alone.
   - `keep`: no log, no metric.

A pod whose annotated VMID does **not** appear in the local runtime list
logs a warning and is left to `CreatePod` to recreate on the next
reconcile.

## VM event watcher

In addition to the periodic probe, vk-cocoon subscribes to cocoon's
real-time VM event stream via `cocoon vm status --event --format json`.
This provides sub-second detection of VM state changes (DELETED, stopped,
error) without waiting for the next probe tick.

The watcher goroutine (`vmWatchLoop`) runs for the lifetime of the
process with automatic restart on subprocess failure (exponential
backoff from 1 s to 60 s, reset on successful connect). Normal stream
closes (cocoon restart) use a fixed 2 s reconnect delay. When an event
arrives:

| Event | Inspect result | Action |
|---|---|---|
| `DELETED` | VM not found | `evictPod`: delete pod (phase=`Failed`, reason=`VMGone`) → operator recreates |
| `MODIFIED` (state ≠ running) | state = stopped/error | `cocoon vm start` (in-place restart, preserves disk/network) |
| `MODIFIED` (state ≠ running) | state = running | False alarm — ignore |

A 30-second **restart cooldown** (`restartCooldown`) prevents tight
restart loops when a VM keeps crashing. If the cooldown has not elapsed
since the last restart, the pod is evicted (phase=`Failed`,
reason=`RestartCooldown`) so the operator can do a clean recreation.
Stale cooldown entries are garbage-collected on each event stream
reconnect.

## Detection latency

| Mechanism | Worst-case latency |
|---|---|
| Probe only (old: 15 s × 5 failures) | ~75 s |
| Probe only (current: 5 s × 3 failures) | ~24 s |
| VM event watcher | **< 1 s** |
