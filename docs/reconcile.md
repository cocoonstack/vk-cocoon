# Runtime reconciliation

Cluster state is the source of truth. vk-cocoon keeps **no** persistent
`pods.json` — it rebuilds its in-memory tables from the Kubernetes API on
every restart, and it reacts to cocoon's live event stream in between.

## Startup reconcile

On every restart vk-cocoon:

1. Lists every pod scheduled to its node via
   `fieldSelector=spec.nodeName=<VK_NODE_NAME>`. `os=macos` pods are
   split out here and adopted via `cocoon-macos vm inspect` plus a PID
   probe (they never appear in `Runtime.List`); a dead or missing record
   is left for the CreatePod replay to restart in place.
2. Lists every VM the cocoon runtime knows about via `Runtime.List`.
3. Routes every record still in `creating` through cocoon's lock-checked
   `vm reconcile-stale-create` verb (needs cocoon > v0.5.7 on the node)
   before any adoption index is built — adopting a creating placeholder
   deadlocks its pod. `collected`/`not-found` free the name for a clean
   recreate; `busy` (an in-flight clone still owns the record) and
   transient verb or inspect errors hand the record to a bounded
   background watcher. Each watcher tick re-invokes the verb:
   `collected`/`not-found` free the name for a clean recreate; otherwise
   an inspect indexes a committed `running` VM or applies the orphan
   policy to a terminal record. An unresolved `creating`/`created` state
   or transient error remains under bounded retry. A record that already
   left `creating` is adopted only when `running`. Every verb attempt is
   counted on
   `cocoon_vk_stale_create_reconcile_total`.
4. Adopts each pod with a `vm.cocoonstack.io/id` annotation by matching
   the VMID against the runtime list.
5. Walks unmatched VMs through the configured `VK_ORPHAN_POLICY`:
   - `destroy` (default): remove the VM and release its DHCP leases so pod-less VMs don't accumulate
     after restart or pod chaos.
   - `alert`: log + bump `cocoon_vk_orphan_vm_total`, and index the VM by name.
   - `keep`: index the VM by name, no log, no metric.

   Both non-destroy policies publish the VM to `vmsByName`, so the next
   `CreatePod` for its pod takes the adopt branch instead of creating a second VM.

6. Dispatches the work a restart interrupted (`dispatchOwedWork`),
   derived per tracked pod from persisted facts only: a hibernate
   annotation with a VM re-enters `hibernate()` (booting a crashed VM
   first); `lifecycle=creating` splits on `post-clone-state` (`running`
   re-runs the fixup, `done` re-checks SAC then holds Ready until the
   lease lands, absent derives the plan — for CH+Windows drop-NIC specs
   hibernate evidence decides restore vs fresh, retried under the
   recheck budget); `failed` stays parked. Every resumed op claims the
   pod for its duration; `UpdatePod` and `DeletePod` back off with an
   error while the claim is held. Dispatches are counted on
   `cocoon_vk_startup_resume_total` and emit `ResumedAfterRestart`.

A pod whose annotated VMID does **not** appear in the local runtime list
logs a warning and is left to `CreatePod` to recreate on the next
reconcile.

Right after the listing and before the stale-create sweep, every
non-terminal pod on the node is checked against the node's snapshot CPU
class (see
[Snapshot CPU compatibility](lifecycle.md#snapshot-cpu-compatibility)); a
mismatch fails startup reconcile, and vk-cocoon refuses to register the
node rather than resume snapshots on an incompatible host.

## Periodic convergence

Every 30 seconds, the status reconciler re-resolves each tracked VM's DHCP
lease and repairs drifted pod status. The same pass idempotently reconciles the
published IP and macOS VNC-port annotations, removing an endpoint when the
runtime no longer serves it; failed patches are retried on the next pass.

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

| Inspect result (after a `DELETED` or non-running `MODIFIED` event) | Action |
|---|---|
| VM not found | `evictPod`: delete pod (phase=`Failed`, reason=`VMGone`) → operator recreates |
| state = stopped/error | `cocoon vm start` (in-place restart, preserves disk/network) |
| state = running | False alarm — ignore |

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
