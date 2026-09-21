# Pod lifecycle

vk-cocoon maps the three pod operations virtual-kubelet delivers onto
cocoon VM operations. Genuine spec changes are handled by the operator
deleting and recreating the pod; vk-cocoon only acts on create, delete,
and the hibernate transition.

## Snapshot CPU compatibility

A pod carrying the `cocoonstack.io/snapshot-cpu-class` node selector
(rendered by cocoon-operator from `spec.snapshotCompatibilityClass`) is
admitted only on a node whose `VK_SNAPSHOT_CPU_CLASS` matches it exactly;
an unclassified node rejects it. The gate runs in `CreatePod`, in
`UpdatePod` so a wake never resumes a memory snapshot on a foreign guest
CPU ABI, and across every pod already scheduled to the node during
[startup reconcile](reconcile.md) — a mismatch there is fatal and the
node never registers, which is the intended outcome for a node whose
class was mis-set under live classified pods. Terminal (`Succeeded` /
`Failed`) pods are exempt: they have no VM to resume, so a leftover pod
cannot wedge node registration. Rejected create/update calls count on
`cocoon_vk_pod_lifecycle_total{reason="snapshot_cpu_class_mismatch"}`.

## CreatePod

1. Parse `meta.VMSpec` from the pod annotations. `Managed` determines
   whether vk-cocoon owns the VM lifecycle. Only managed `os=macos` pods
   dispatch to [cocoon-macos](#macos-guests); other managed pods use
   Cloud Hypervisor or Firecracker. Unmanaged pods use the operator's
   pre-assigned runtime annotations and skip guest setup.
2. If a VM with `spec.VMName` already exists locally, adopt it
   (idempotent on restart). Adoption hinges on `StartupReconcile` having
   populated `vmsByName`, which is why `main.go` runs it to completion
   before the pod controller starts.
3. Otherwise `bringUpVM` selects a path. An unmanaged pod (`spec.Managed`
   false) short-circuits first onto its pre-assigned VMID/IP; the managed
   paths are then tried in order — restore-from-hibernate, clone-from-dir,
   fork-from, and finally `spec.Mode`:
   - **Restore-from-hibernate**: taken when the operator set
     `vm.cocoonstack.io/restore-from-hibernate` (a cross-node wake
     arriving via CreatePod rather than UpdatePod), **or derived from
     evidence**: a managed pod with no marker whose VM name still owns a
     `:hibernate` registry tag (local snapshot presence in registry-less
     deployments) is a wake lost to a vk restart, and fresh-booting it
     would let the next hibernate overwrite the guest's state. Both the
     marked and the derived path fail closed on registry errors
     (`HibernateEvidenceUnavailable`) and reject a pod whose image ref
     differs from the ref recorded on the hibernate artifact at push time
     (`cocoonstack.snapshot.baseimage`); only the derived path conflicts
     with explicit clone sources, emits `HibernateSnapshotExists` and
     counts `verdict=restored`. Identity is compared by ref:
     a ref change signals operator intent for a different image, while
     content drift under an unchanged ref is governed by the
     hibernate-state-is-authoritative contract (discarding hibernated
     state requires deleting the tag). Registry-less deployments have no
     identity guard. The derived marker stays in-memory — a later restart
     re-derives it. Then pull the `:hibernate` snapshot and clone from
     it, same as the UpdatePod wake path.
   - **Fork-from** (`spec.ForkFrom`): snapshot the named source VM once
     (deduped via `ensureForkSnapshot`) and clone every fork off that
     shared snapshot. The fork snapshot is a per-lineage baseline: every
     fresh (non-restore) bring-up drops `fork-<vm>` before booting, so a
     recreated same-name VM can never hand a dead incarnation's baseline
     to new sub-agents; a hibernate restore keeps it, since a wake
     continues the same lineage.
   - **`Managed=false`** (static / externally-managed VMs, e.g. Windows
     toolboxes on an external QEMU host): skip the runtime entirely and
     adopt the pre-assigned `VMID` / `IP` / `VNCPort` the operator
     pre-wrote into the `VMRuntime` annotations. `Managed` is the single
     source of truth for "vk-cocoon owns this VM's lifecycle": a
     hibernate annotation on such a pod is a no-op, delete only forgets
     the pod, a restart re-adopts it from those annotations and
     re-publishes Ready without any guest work, the VM event watcher
     leaves its VM alone, and `os=macos` on such a pod never enters the
     cocoon-macos lifecycle.
   - **Mode `clone`** (default, `Managed=true`): look up the snapshot
     locally using a **tag-aware name** (`repo:tag`, or bare `repo` when
     the tag is `latest` for backward compatibility; a name over 63
     characters is truncated to a sha256-suffixed form). If the local
     snapshot does not exist, pull it from the registry via
     `Puller.PullSnapshot`. Before cloning, `assertSnapshotBackend`
     validates the snapshot's recorded hypervisor matches `spec.Backend`
     — a CH snapshot cannot be cloned onto a FC target and vice-versa.
     When the snapshot carries a base image, `Pull: true` is passed to
     `CloneOptions`, which translates to `cocoon vm clone --pull`; cocoon
     constructs a digest reference (`repo@sha256:xxx`) from the snapshot
     metadata and pulls the exact image version recorded at snapshot
     time. Then `Runtime.Clone(from=<local>, to=spec.VMName)`. Pod-side
     guest topology (vCPU count/memory/storage) is not plumbed into
     clone — cocoon clone inherits it from the snapshot. Host-side
     cgroup CPU policy is: cocoon never inherits cgroup knobs from a
     snapshot, so every clone path passes `--cpu-weight` (from the
     pod's requests via kubelet's cgroup v2 conversion, minimum 1 for
     BestEffort) and, only when the pod has a CPU limit,
     `--cpu-quota-us` with `--cpu-period-us`; without a limit cocoon's
     Guaranteed-at-N quota applies. Only the `vm run` path additionally
     translates pod resources into guest resources: vCPU count rounds
     the CPU limit up (requests when no limit is set), and `--memory`
     is taken from the pod's memory request, falling back to the limit
     when no request is set.
   - **Mode `run`** (`Managed=true`): `ensureRunImage` makes the image
     available locally before launching the VM. It peeks the OCI
     manifest via `Puller.Registry`: cocoonstack cloud-image artifacts
     (artifactType=`application/vnd.cocoonstack.os-image.v1+json`) take
     the qcow2 streaming path through `Puller.EnsureCloudImageFromRaw` →
     `cocoon image import`, snapshot artifacts are rejected with a "use
     mode=clone" error, and everything else (HTTP(S) URLs, container
     images, refs that don't resolve against the registry) falls through
     to `Runtime.EnsureImage` → `cocoon image pull`. `--force` when
     `spec.ForcePull` is true. Then `Runtime.Run(image=spec.Image,
     name=spec.VMName)`. When `spec.Backend` is `firecracker`, `--fc`
     selects the FC backend; when `spec.OS` is `windows`, `--windows` is
     passed. When `spec.NoDirectIO` is true, `--no-direct-io` disables
     O_DIRECT on writable disks (CH only, useful for dev/test).
   - **`vm.cocoonstack.io/clone-from-dir` override** (managed-only, takes
     precedence over mode/fork-from): clone via `cocoon vm clone
     --from-dir <abs-path> --pull`, bypassing the local snapshot DB.
     Pairs with `cocoon snapshot export --to-dir` for cross-node staging.
     Conflicts with `mode=run` or `fork-from` fast-fail.
4. Resolve the IP from the cocoon-net JSON lease file by MAC.
5. `meta.VMRuntime{VMID, IP}.Apply(pod)` writes the runtime annotations
   back so the operator and other consumers can pick them up. `VNCPort`
   stays unset on this path — cloud-hypervisor has no VNC server; the
   macOS path and the pre-seeded static-toolbox path publish a non-zero
   value.
6. For clone/fork/wake paths that need guest-side network setup (see
   [Post-clone hints](post-clone.md)), dispatch the fixup in the background
   after the annotations are written. vk-cocoon runs it itself over
   `cocoon vm exec`, retrying every 3 s within a 180 s budget, then waits
   for an IP before publishing ready intent; PodReady also requires a
   successful probe. Only when that budget is exhausted does it write the
   commands as a base64-encoded annotation
   (`vm.cocoonstack.io/post-clone-hint`), emit `PostCloneExecExhausted`, and
   mark lifecycle Failed. Manual guest repair alone does not clear that
   state; see [recovery](post-clone.md#recovery-after-exhaustion).
7. Launch a per-pod probe agent (see [Readiness probing](probes.md)). The
   agent's first probe runs synchronously so the initial `notify` push
   already reflects reachability; later probes run on a ticker and call
   back into the provider whenever readiness flips so the async notify
   hook re-fires. Kubernetes `Ready=True` requires both a successful probe
   and vk's lifecycle intent reaching `ready`, so early reachability cannot
   expose a clone; on the apiserver the status patch lands before the
   `lifecycle-state=ready` annotation patch.

## DeletePod

1. Decode `meta.VMSpec`. Unmanaged pods are only forgotten; their VMs
   remain under the external owner's control. Managed `os=macos` pods
   tear down via `cocoon-macos vm rm`, release their DHCP leases, and
   skip the snapshot logic below.
2. `meta.ShouldSnapshotVM(spec, meta.RoleForPod(pod, spec.VMName))` — the
   shared cocoon-common decoder — decides whether to snapshot before
   destroy. The role comes from the pod's CocoonSet owner (via
   `RoleForPod`), not a VM-name suffix heuristic, so a toolbox named with
   a trailing `-0` is never mistaken for the main agent:
   - `always`: `Runtime.SnapshotSave` then
     `Pusher.PushSnapshot(tag=meta.DefaultSnapshotTag)` to the registry.
   - `main-only`: same, but only for the main agent (role `RoleMain`,
     i.e. slot 0 of its CocoonSet).
   - `never`: skip snapshots entirely.
3. `Runtime.Remove(vmID)` destroys the VM; an already absent VM also
   completes this step. Then release each DHCP-backed NIC lease through
   cocoon-net's local control socket. Lease cleanup is best-effort after
   destruction; the normal lease expiry remains
   the fallback if cocoon-net is temporarily unavailable.
4. Drop the local snapshot and its fork snapshot, **unless** the pod carries
   `vm.cocoonstack.io/keep-snapshot-on-delete`. The operator sets that flag
   when the delete is a `hibernatePolicy: release` seat release: the VM state
   stays claimable from the `:hibernate` tag, so the node-local snapshot is
   kept as the warm-wake cache that lets a wake landing back on this node
   skip the registry pull. `resolveWakeSource` still verifies any local copy
   against the tag's `SnapshotID`, so keeping it cannot restore stale state.
   A missing flag only costs a pull, never correctness.
5. Forget the pod from the in-memory tables.

## UpdatePod

The only update vk-cocoon honors is a `HibernateState` transition.
Anything else is a no-op (the operator deletes and recreates the pod for
genuine spec changes). Unmanaged pods ignore hibernate regardless of OS.
Managed `os=macos` pods reject hibernate because cocoon-macos snapshots
are offline disk snapshots with no live save/restore.

| Transition | Behavior |
|---|---|
| `false → true` | NetResize (CH+Windows) → SnapshotSave → Push → clear VMID before Remove → Remove, then release the guest's DHCP leases through cocoon-net (rollback on failure precedes this post-remove lease cleanup). The pod object stays, reporting phase `Pending` while no VM is tracked, so K8s controllers do not recreate it. VMID/IP annotations clear between Push and Remove so the operator's manifest+VMID race window collapses to one patch RTT. **Compensating rollback**: if `Runtime.Remove` fails after a successful push, vk-cocoon best-effort `Registry.DeleteManifest` the hibernate tag and re-applies VMID/IP so the pod stays recoverable. Push and Save are idempotent, so a compensated retry re-publishes the tag cleanly on the next attempt. |
| `true → false` (with no live VM) | Resolve the clone source in order: registry-verified local snapshot → best-effort raw-file restore from the manifest's `from-node` peer → registry `Puller.PullSnapshot(tag=meta.HibernateSnapshotTag)`. Peer files are staged for `Runtime.Clone --from-dir`; an unavailable peer, checksum failure, or snapshot-ID mismatch falls through to the registry path. CH+Windows snapshots are NIC-less, so the clone hot-adds a fresh NIC; vk-cocoon then waits up to 45 s for a DHCP lease, nudging a renew at 30 s if none has landed yet. vk-cocoon does not touch the registry tag on wake; the operator's `CocoonHibernation` reconciler drops the `:hibernate` tag once the woken VM is running. |

On Save, Push, or Remove failure after a NIC drop, rollback re-adds the NIC,
inspects its current MAC and network configuration, and republishes VMID/IP.
DHCP addresses are cleared until the replacement NIC's lease resolves;
static NICs retain the fresh inspected address. Rollback has a bounded
lifetime independent of the failed request's cancellation.

The operator's `CocoonHibernation` reconciler tracks the transition by
polling the registry for the `hibernate` manifest.

## macOS guests

Managed pods annotated `cocoonset.cocoonstack.io/os: macos` dispatch to the standalone
[cocoon-macos](https://github.com/cocoonstack/cocoon-macos) QEMU/KVM backend
(`VK_COCOON_MACOS_BIN`, default `/usr/local/bin/cocoon-macos`) instead of the
cocoon CLI. The guest joins the cocoon CNI plane for a DHCP'd routed IP.
Readiness uses a bare TCP accept on the declared `vm.cocoonstack.io/probe-port`,
or falls back to requiring the guest sshd's `SSH-` banner on `:22`. The QEMU VNC
framebuffer gets a node-unique, password-protected host port allocated by
vk-cocoon, published via `vm.cocoonstack.io/vnc-port` and served on all node
interfaces (firewall 5900-5999; unset `COCOON_MACOS_VNC_PASSWORD` disables VNC).
A vk-cocoon restart
adopts a live guest instead of relaunching it (two QEMU processes on one overlay
corrupt the disk). Hibernate/wake, fork, and snapshot push do not apply to macOS guests.
