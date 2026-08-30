# Readiness probing

vk-cocoon implements v-k's `NotifyPods` interface, so the framework
treats it as an **async provider**: Kubernetes only sees the pod status
vk-cocoon actively pushes through `notify`, and v-k never polls
`GetPodStatus` on its own. That makes a real per-pod probe loop
load-bearing — any status change that happens after `CreatePod` returns
is invisible to the cluster unless vk-cocoon re-fires `notify`.

A successful probe is necessary but not sufficient for `Ready=True`: vk's
in-memory lifecycle intent must also be `ready`. This keeps a reachable VM Not
Ready while clone setup runs, after lifecycle failure, or during hibernation.
On the apiserver the status patch carrying `Ready=True` lands before the
`lifecycle-state=ready` annotation patch — consumers gate on the annotation,
not the reverse.

## The probe loop

The `probes/` package owns that loop:

1. `CreatePod` (and startup reconcile) call `Manager.Start(key, probe,
   onUpdate)`. The probe closure the provider supplies performs three
   checks in order:
   1. The tracked VM still exists.
   2. For DHCP-backed VMs, re-read the cocoon-net lease file by MAC on
      every tick and write the current address back via `setVMIP`; the
      tracked IP is only a cache. Static NICs keep their inspected IP.
   3. If the pod carries a `vm.cocoonstack.io/probe-port` annotation, dial
      TCP on that port instead of ICMP. Otherwise fall back to
      `Pinger.Ping(ctx, ip)` — a single ICMPv4 echo. This matches the
      cocoon Windows golden image contract
      (`windows/autounattend.xml` explicitly opens `icmpv4:8` and disables
      all firewall profiles), and it decouples readiness from specific
      services so the same probe works for Linux and Windows guests alike.

   `os=macos` pods get a different closure: when
   `vm.cocoonstack.io/probe-port` is declared, the probe considers a bare TCP
   accept on that port ready. Otherwise it falls back to the guest's sshd on
   `:22` and requires the `SSH-` banner — a cold macOS boot takes minutes and
   its DHCP lease appears long before sshd answers. On finding the QEMU process
   dead, it restarts the record in place (rate-limited); nothing else can see a
   crashed QEMU guest.
2. The first probe runs **synchronously inside `Start`** so the
   refreshStatus/notify pass that `CreatePod` does before returning
   already reflects the initial reachability decision.
3. A background goroutine re-runs the probe — pre-Ready on exponential
   backoff (100 ms growing ×1.5 up to a 1 s cap, so a fast guest flips to
   Ready within ~100 ms), then a steady 5 s interval once Ready — and
   invokes `onUpdate` after 3 consecutive failures flip readiness back to
   false. `onUpdate` re-reads the pod, rebuilds the status, and calls
   `notify` so the kubelet observes the change.
4. `DeletePod` calls `Manager.Forget`, which cancels the per-pod
   goroutine; `Manager.Close` is called once at shutdown to tear every
   remaining agent down.

## Degraded mode without CAP_NET_RAW

If the ICMP raw socket cannot be opened — typically because the binary is
running without `CAP_NET_RAW` — the provider falls back to
`network.NopPinger` and the probe degrades to "an IP was resolved ==
Ready". That is weaker than a real end-to-end ping but still strictly
better than the previous behaviour of marking the pod Ready the instant
`cocoon vm clone/run` returned. The systemd unit in
`packaging/vk-cocoon.service` grants `AmbientCapabilities=CAP_NET_RAW` so
the production path gets the real pinger.
