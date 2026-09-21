# Post-clone network hints

After a clone (or hibernate wake / fork), vk-cocoon checks whether the VM
needs manual guest-side network setup. CH clones are automatic on all-DHCP
networks (NIC hot-swap triggers systemd-networkd to re-DHCP); the fixup path
is entered when a static-IP NIC is present, for every Firecracker clone
(the guest MAC is frozen in vmstate), and for every managed `os=windows` pod. These combinations require intervention:

| Scenario | Reason | Hint commands |
|---|---|---|
| CH + cloudimg + static IP | snapshot restore does not re-trigger cloud-init | `cloud-init clean + init` |
| CH + OCI + static IP | guest retains old IP config | write MAC-based networkd files |
| FC (any) | guest MAC frozen in vmstate | `ip link set address` + networkd reconfig |
| Windows (any managed) | guest NIC needs a Plug-and-Play re-enumerate to come up cleanly | PowerShell `Disable-PnpDevice` + `Enable-PnpDevice` on Class Net |

vk-cocoon first applies the fixup itself over `cocoon vm exec`, retrying
every 3 s within a 180 s budget. After successful setup it waits for an IP
before publishing ready intent; PodReady also requires a successful probe.
CH+Windows hibernate restores use the fresh NIC's IP-wait path directly.
Once the setup budget is exhausted, it base64-encodes the required shell
commands into `vm.cocoonstack.io/post-clone-hint` on the pod, records the
joined per-attempt error chain in `vm.cocoonstack.io/post-clone-errors`,
and emits a warning, setting lifecycle Failed and leaving the pod Not Ready.
To retrieve the commands:

```bash
kubectl get pod <name> \
  -o jsonpath='{.metadata.annotations.vm\.cocoonstack\.io/post-clone-hint}' \
  | base64 -d
```

Classification uses the snapshot's original image URL (normal clone) or
the COW file type on disk (fork/wake) to distinguish cloudimg from OCI.

## Recovery after exhaustion

Executing the hint through `cocoon vm exec` can restore guest connectivity,
but a successful probe does not clear lifecycle Failed or make the pod Ready.
Startup reconciliation also leaves failed post-clone work parked.
After correcting the guest or image configuration, recreate the managed pod
through its owning controller to start a new setup attempt. Recreation follows
the normal [deletion and snapshot policy](lifecycle.md#deletepod); it is not an
in-place retry of the manually repaired VM.
