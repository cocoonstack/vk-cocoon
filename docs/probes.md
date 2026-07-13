# Readiness probing

vk-cocoon implements v-k's `NotifyPods` interface, so the framework
treats it as an **async provider**: Kubernetes only sees the pod status
vk-cocoon actively pushes through `notify`, and v-k never polls
`GetPodStatus` on its own. That makes a real per-pod probe loop
load-bearing — any status change that happens after `CreatePod` returns
is invisible to the cluster unless vk-cocoon re-fires `notify`.

## The probe loop

The `probes/` package owns that loop:

1. `CreatePod` (and startup reconcile) call `Manager.Start(key, probe,
   onUpdate)`. The probe closure the provider supplies performs three
   checks in order:
   1. The tracked VM still exists.
   2. If the in-memory VM record has no IP, re-try the cocoon-net lease
      file by MAC and write it back via `setVMIP`.
   3. If the pod carries a `vm.cocoonstack.io/probe-port` annotation, dial
      TCP on that port instead of ICMP. Otherwise fall back to
      `Pinger.Ping(ctx, ip)` — a single ICMPv4 echo. This matches the
      cocoon Windows golden image contract
      (`windows/autounattend.xml` explicitly opens `icmpv4:8` and disables
      all firewall profiles), and it decouples readiness from specific
      services so the same probe works for Linux and Windows guests alike.
2. The first probe runs **synchronously inside `Start`** so the
   refreshStatus/notify pass that `CreatePod` does before returning
   already reflects the initial reachability decision.
3. A background goroutine re-runs the probe on a ticker (2 s cold-start,
   5 s once Ready) and invokes `onUpdate` after 3 consecutive failures
   flip readiness back to false. `onUpdate` re-reads the pod, rebuilds the
   status, and calls `notify` so the kubelet observes the change.
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
