# Metrics & monitoring

vk-cocoon exposes three metrics surfaces. The kubelet API port below
defaults to `10250` and is set by `VK_KUBELET_PORT`.

## `:10250/stats/summary` — kubelet stats API

Consumed by metrics-server and `kubectl top`. Reports per-pod CPU
(cumulative nanoseconds) from the VM's cocoon cgroup scope
(`<COCOON_CGROUP_PARENT>/vm-<id>.scope/cpu.stat` `usage_usec` — unlike
the VMM's utime/stime, it includes the virtio and io_uring kernel
workers the scope contains) and memory (RSS field 24) from
`/proc/<pid>/stat`, plus per-pod network I/O from the TAP device
inside each VM's network namespace (`/proc/<pid>/net/dev`). Node-level
CPU and memory are read from `/proc/stat` and `/proc/meminfo`.

## `:10250/metrics/resource` — metrics-server resource endpoint

Prometheus text format with the metric families metrics-server and HPA
require: `node_cpu_usage_seconds_total`,
`node_memory_working_set_bytes`, `container_cpu_usage_seconds_total`,
`container_memory_working_set_bytes`, `pod_cpu_usage_seconds_total`,
`pod_memory_working_set_bytes`, plus the cAdvisor-style throttling
counters `container_cpu_cfs_throttled_seconds_total` and
`container_cpu_cfs_throttled_periods_total` from the VM scope's
`cpu.stat` (`throttled_usec` / `nr_throttled`) — who is hitting their
CPU quota.

## `:9091/metrics` — vk-cocoon metrics

Prometheus endpoint with vk-cocoon-specific metrics:

| Metric | Type | Description |
|---|---|---|
| `cocoon_vk_vm_cpu_seconds_total{vm,pod,namespace,backend}` | Counter | Per-VM cumulative CPU (cgroup scope `usage_usec`) |
| `cocoon_vk_vm_cpu_throttled_seconds_total{vm,pod,namespace,backend}` | Counter | Per-VM time throttled by the CPU quota |
| `cocoon_vk_vm_cpu_throttled_periods_total{vm,pod,namespace,backend}` | Counter | Per-VM throttled enforcement periods |
| `cocoon_vk_vm_memory_rss_bytes{vm,pod,namespace,backend}` | Gauge | Per-VM RSS |
| `cocoon_vk_vm_disk_cow_bytes{vm,pod,namespace,backend}` | Gauge | Per-VM COW overlay actual size |
| `cocoon_vk_vm_network_rx_bytes_total` / `tx_bytes_total` | Counter | Per-VM TAP network I/O |
| `cocoon_vk_node_cpu_seconds_total` | Counter | Node cumulative CPU |
| `cocoon_vk_node_memory_used_bytes` | Gauge | Node used memory |
| `cocoon_vk_node_storage_available_bytes` / `total_bytes` | Gauge | Cocoon root filesystem |
| `cocoon_vk_vm_boot_duration_seconds{mode,backend}` | Histogram | VM creation time (run or clone) |
| `cocoon_vk_snapshot_save_duration_seconds` | Histogram | Snapshot save time |
| `cocoon_vk_snapshot_push_duration_seconds` | Histogram | Registry push time |
| `cocoon_vk_snapshot_pull_duration_seconds` | Histogram | Registry pull time |
| `cocoon_vk_snapshot_peer_restore_duration_seconds` | Histogram | Time to stage raw snapshot files from a peer node |
| `cocoon_vk_probe_duration_seconds` | Histogram | Per-probe health check time (ICMP or TCP) |
| `cocoon_vk_pod_lifecycle_total{op,result,reason}` | Counter | Pod lifecycle operations (`result=ok\|failed\|skipped`, `reason` sub-classifies) |
| `cocoon_vk_snapshot_pull_total{result}` / `save_total` / `push_total` | Counter | Snapshot pull/save/push counts |
| `cocoon_vk_snapshot_peer_restore_total{result}` | Counter | Peer restore outcomes (`result=ok\|failed`); failures fall through to the registry pull |
| `cocoon_vk_snapshot_verify_total{result}` | Counter | Wake-time verification of the local snapshot against the registry tag (`result=ok\|stale\|error`); `stale` discards the local copy and pulls, `error` fails the wake rather than trust an unverified copy |
| `cocoon_vk_clone_from_dir_total{result}` | Counter | Annotation-driven `--from-dir` clone attempts |
| `cocoon_vk_hibernate_total{phase,result}` | Counter | Hibernate stage outcomes (`phase=dhcp_release\|netresize\|snapshot\|push\|remove`; `dhcp_release` is the guest-side `ipconfig /release` on the CH+Windows drop-NIC path — the host-side cocoon-net lease release is counted by `cocoon_vk_lease_release_total`) |
| `cocoon_vk_wake_total{result}` | Counter | Wake operation outcomes |
| `cocoon_vk_wake_ip_wait_total{result}` | Counter | Post-clone and wake DHCP-lease-wait outcomes — both the CH+Windows dropNIC wake and every clone's post-clone IP wait (`result=ok\|timeout`) |
| `cocoon_vk_wake_renew_nudge_total{result}` | Counter | `ipconfig /renew` nudges sent to Windows guests still lease-less mid lease-wait (`result=ok\|failed`; failed means the exec didn't confirm — the in-guest renew may still have taken effect, the lease re-check decides) |
| `cocoon_vk_postclone_total{kind,result}` | Counter | Post-clone fixup outcomes (`kind=linux_static\|linux_fc\|windows\|sac`) |
| `cocoon_vk_postclone_retry_attempts{result}` | Histogram | Attempts consumed before post-clone exec succeeded or failed (`result=ok\|failed`) |
| `cocoon_vk_vm_table_size` | Gauge | Tracked VM count |
| `cocoon_vk_orphan_vm_total` | Counter | Orphan VMs at startup |
| `cocoon_vk_vm_inspect_transient_fail_total` | Counter | Transient VM inspect failures tolerated by the status refresher |
| `cocoon_vk_pod_evict_failure_total` | Counter | Failed pod evictions |
| `cocoon_vk_reconcile_adopt_by_name_total` | Counter | Startup reconcile adoptions matched by VM name |
| `cocoon_vk_stale_create_reconcile_total{outcome}` | Counter | Stale-create verb attempts by startup reconcile and its bounded watcher (`outcome=collected\|busy\|not-creating\|not-found\|error`) |
| `cocoon_vk_hibernate_evidence_total{verdict}` | Counter | Fresh boots intercepted by hibernate evidence (`verdict=restored\|image_conflict\|source_conflict\|unavailable`) |
| `cocoon_vk_startup_resume_total{op}` | Counter | Interrupted operations re-dispatched by startup reconcile (`op=hibernate\|post_clone\|ready_wait\|classify_drop_nic`) |

All three metrics surfaces share one complete sample for two seconds,
so reported values may trail the kernel by up to that interval. Per-VM
CPU and throttling come from the VM's cocoon cgroup scope; RSS and
network still use the hypervisor PID tracked in memory — no shell-out
to `cocoon` on each scrape. The tracking table is snapshot-copied under
RLock and the `/proc` + cgroup reads happen outside the lock to avoid
blocking CreatePod/DeletePod. When a VM is restarted in-place (event
watcher → `cocoon vm start`), the PID is re-inspected and refreshed.

## Kubernetes Events

In addition to metrics, the hibernate / wake / post-clone failure paths
write a K8s Event on the Pod with a typed Reason — `kubectl describe pod`
surfaces the same signal that `vm.cocoonstack.io/lifecycle-state-message`
carries. Spec-validation rejects (e.g. missing `vm.cocoonstack.io/name`)
and pod-delete short-circuits stay counter-only on `pod_lifecycle_total`:
they are input-validation noise rather than runtime-lifecycle failures,
and the rejection is already visible to the caller as the synchronous
error return.

- **Warning reasons**: `CreateBringUpFailed`, `HibernateEvidenceFailed`,
  `HibernateEvidenceUnavailable`, `HibernateSnapshotExists`,
  `HibernateNetResizeFailed`,
  `HibernateSnapshotFailed`, `HibernatePushFailed`,
  `HibernateRemoveFailed`, `WakePullFailed`, `WakeCloneFailed`,
  `WakeIPWaitTimeout`, `WindowsStaticIPFailed`,
  `PostCloneIPWaitTimeout`,
  `PostCloneExecAttemptFailed`, `PostCloneExecExhausted`,
  `PostCloneSACDialFailed`, `PostCloneSACEnumFailed`,
  `PostCloneSACSetFailed`, `PostCloneSACVerifyFailed`,
  `ResumeStartFailed`, `ResumeClassifyFailed`.
- **Normal reasons**: `Hibernated`, `Woken`, `PostCloneSucceeded`,
  `ResumedAfterRestart`.
