# Pod lifecycle

vk-cocoon maps the three pod operations virtual-kubelet delivers onto
cocoon VM operations. Genuine spec changes are handled by the operator
deleting and recreating the pod; vk-cocoon only acts on create, delete,
and the hibernate transition.

## CreatePod

1. Parse `meta.VMSpec` from the pod annotations.
2. If a VM with `spec.VMName` already exists locally, adopt it
   (idempotent on restart). Adoption hinges on `StartupReconcile` having
   populated `vmsByName`; before reconcile completes, CreatePod treats
   the pod as new and may collide on VM name.
3. Otherwise `bringUpVM` selects a path — restore-from-hibernate and
   fork-from take precedence, then `spec.Managed`, then `spec.Mode`:
   - **Restore-from-hibernate**: taken when the operator set
     `vm.cocoonstack.io/restore-from-hibernate` (a cross-node wake
     arriving via CreatePod rather than UpdatePod), **or derived from
     evidence**: a managed pod with no marker whose VM name still owns a
     `:hibernate` registry tag (local snapshot presence in registry-less
     deployments) is a wake lost to a vk restart, and fresh-booting it
     would let the next hibernate overwrite the guest's state. The
     derived path fails closed on registry errors
     (`HibernateEvidenceUnavailable`), conflicts loudly with explicit
     clone sources, and rejects a pod whose image ref differs from the
     ref recorded on the hibernate artifact at push time
     (`cocoonstack.snapshot.baseimage`). Identity is compared by ref:
     a ref change signals operator intent for a different image, while
     content drift under an unchanged ref is governed by the
     hibernate-state-is-authoritative contract (discarding hibernated
     state requires deleting the tag). Registry-less deployments have no
     identity guard. The derived marker stays in-memory — a later restart
     re-derives it. Then pull the `:hibernate` snapshot and clone from
     it, same as the UpdatePod wake path.
   - **Fork-from** (`spec.ForkFrom`): snapshot the named source VM once
     (deduped via `ensureForkSnapshot`) and clone every fork off that
     shared snapshot.
   - **`Managed=false`** (static / externally-managed VMs, e.g. Windows
     toolboxes on an external QEMU host): skip the runtime entirely and
     adopt the pre-assigned `VMID` / `IP` / `VNCPort` the operator
     pre-wrote into the `VMRuntime` annotations. `Managed` is the single
     source of truth for "vk-cocoon owns this VM's lifecycle".
   - **Mode `clone`** (default, `Managed=true`): look up the snapshot
     locally using a **tag-aware name** (`repo:tag`, or bare `repo` when
     the tag is `latest` for backward compatibility). If the local
     snapshot does not exist, pull it from the registry via
     `Puller.PullSnapshot`. Before cloning, `assertSnapshotBackend`
     validates the snapshot's recorded hypervisor matches `spec.Backend`
     — a CH snapshot cannot be cloned onto a FC target and vice-versa.
     When the snapshot carries a base image, `Pull: true` is passed to
     `CloneOptions`, which translates to `cocoon vm clone --pull`; cocoon
     constructs a digest reference (`repo@sha256:xxx`) from the snapshot
     metadata and pulls the exact image version recorded at snapshot
     time. Then `Runtime.Clone(from=<local>, to=spec.VMName)`. Pod-side
     CPU/memory/storage are not plumbed into clone — cocoon clone
     inherits all guest resources from the snapshot. Only the `vm run`
     path translates pod resources into VM resources.
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
4. For clone/fork/wake paths, check whether the VM needs manual network
   setup (see [Post-clone hints](post-clone.md)). If so, write the
   required commands as a base64-encoded annotation
   (`vm.cocoonstack.io/post-clone-hint`) and log a warning. The pod stays
   Running but Not Ready until the user executes the commands via `cocoon
   vm console` and the probe detects network connectivity.
5. Resolve the IP from the cocoon-net JSON lease file by MAC.
6. `meta.VMRuntime{VMID, IP}.Apply(pod)` writes the runtime annotations
   back so the operator and other consumers can pick them up. `VNCPort`
   is intentionally left unset here — cloud-hypervisor has no VNC server,
   so only the pre-seeded static-toolbox path ever carries a non-zero
   value.
7. Launch a per-pod probe agent (see [Readiness probing](probes.md)). The
   agent's first probe runs synchronously so the initial `notify` push
   already reflects reachability; later probes run on a ticker and call
   back into the provider whenever readiness flips so the async notify
   hook re-fires.

## DeletePod

1. Decode `meta.VMSpec`.
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
3. `Runtime.Remove(vmID)` to destroy the VM.
4. Forget the pod from the in-memory tables.

## UpdatePod

The only update vk-cocoon honors is a `HibernateState` transition.
Anything else is a no-op (the operator deletes and recreates the pod for
genuine spec changes).

| Transition | Behavior |
|---|---|
| `false → true` | NetResize (CH+Windows) → SnapshotSave → Push → clear VMID before Remove → Remove (rollback on failure). Pod stays alive (`PodRunning`) so K8s controllers do not recreate it. VMID/IP annotations clear between Push and Remove so the operator's manifest+VMID race window collapses to one patch RTT. **Compensating rollback**: if `Runtime.Remove` fails after a successful push, vk-cocoon best-effort `Registry.DeleteManifest` the hibernate tag and re-applies VMID/IP so the pod stays recoverable. Push and Save are idempotent, so a compensated retry re-publishes the tag cleanly on the next attempt. |
| `true → false` (with no live VM) | `Puller.PullSnapshot(tag=meta.HibernateSnapshotTag)` → `Runtime.Clone`. vk-cocoon does not touch the registry tag on wake; the operator's `CocoonHibernation` reconciler drops the `:hibernate` tag once the woken VM is running. |

The operator's `CocoonHibernation` reconciler tracks the transition by
polling the registry for the `hibernate` manifest.
