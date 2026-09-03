# Post-clone network hints

After a clone (or hibernate wake / fork), vk-cocoon checks whether the VM
needs manual guest-side network setup. CH clones are automatic on all-DHCP
networks (NIC hot-swap triggers systemd-networkd to re-DHCP); the fixup path
is entered when a static-IP NIC is present, and unconditionally for every
`os=windows` pod. These combinations require intervention:

| Scenario | Reason | Hint commands |
|---|---|---|
| CH + cloudimg + static IP | snapshot restore does not re-trigger cloud-init | `cloud-init clean + init` |
| CH + OCI + static IP | guest retains old IP config | write MAC-based networkd files |
| FC (any) | guest MAC frozen in vmstate | `ip link set address` + networkd reconfig |
| Windows (any) | guest NIC needs a Plug-and-Play re-enumerate to come up cleanly | PowerShell `Disable-PnpDevice` + `Enable-PnpDevice` on Class Net |

vk-cocoon first applies the fixup itself over `cocoon vm exec`, retrying
every 3 s within a 180 s budget and flipping the pod Ready on success. Only
once that budget is exhausted does it base64-encode the required shell
commands into `vm.cocoonstack.io/post-clone-hint` on the pod, record the
joined per-attempt error chain in `vm.cocoonstack.io/post-clone-errors`,
and emit a warning, leaving the pod Running but Not Ready. To retrieve the
commands:

```bash
kubectl get pod <name> \
  -o jsonpath='{.metadata.annotations.vm\.cocoonstack\.io/post-clone-hint}' \
  | base64 -d
```

After executing via `cocoon vm exec`, the probe detects connectivity
and flips Ready automatically.

Classification uses the snapshot's original image URL (normal clone) or
the COW file type on disk (fork/wake) to distinguish cloudimg from OCI.
