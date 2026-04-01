# Design

`vk-cocoon` treats the Kubernetes pod object as the control-plane record for a MicroVM.

## Core Model

1. the pod carries Cocoon-specific annotations
2. the provider resolves the desired VM identity and source image
3. missing snapshots are pulled from `epoch`
4. Cocoon boots or clones the VM on the target worker
5. the provider publishes live metadata back to the pod annotations

## Restart Recovery

Provider restart must not create duplicate VMs.

Recovery rules:

1. derive the stable VM identity from `cocoon.cis/vm-id` and `cocoon.cis/vm-name`
2. inspect existing Cocoon runtime state before calling create/clone
3. if the VM or suspended snapshot already exists, reattach in-memory state
4. only create a new VM when no recoverable runtime state exists

## Networking

The provider distinguishes between:

- host-side network bookkeeping from the hypervisor/runtime
- guest-visible IP addresses obtained inside the VM

For managed guests, the provider should publish the guest-reachable address back to:

- `status.podIP`
- `cocoon.cis/ip`

It should not confuse that guest IP with the virtual node host IP.

## Hibernation

`vk-cocoon` does not expose a separate VM CRD. Instead:

- `cocoon-operator` annotates pods for hibernation
- the provider snapshots the VM, destroys runtime state, and leaves the pod object alive
- waking the pod restores the VM from the saved snapshot

This preserves Kubernetes ownership semantics while keeping VM state outside the container abstraction.

## Windows

Windows support follows the same managed lifecycle as Linux:

- the pod is represented by a Cocoon-managed VM
- the provider uses the Windows-specific runtime branch when the pod is annotated with `cocoon.cis/os=windows`
- guest metadata is refreshed from live runtime state rather than from stale manifest hints

The design goal is that Windows is first-class in the Cocoon lifecycle, not an external static desktop registration.
