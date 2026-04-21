# RWX DaemonSet Rewrite — Design

Status: **DRAFT, NOT IMPLEMENTED** — for review before coding.
Replaces the current per-PVC Ganesha Deployment ([internal/nfs/manifests.go](../internal/nfs/manifests.go), [internal/controller/rwx_pvc_controller.go](../internal/controller/rwx_pvc_controller.go)) with a one-NFS-server-per-node DaemonSet, inspired by Piraeus's `linstor-csi-nfs-server` pattern.

---

## Motivation

The current RWX implementation creates **one NFS Ganesha Deployment per RWX PVC**. Properties:

- Isolation: a bad client affects only one PVC's server
- Unprivileged: no host access, no privileged containers
- Simple lifecycle: PVC create → Deploy + Service + PV; PVC delete → GC via owner references

Problems at scale:

- Pod count grows as O(N_RWX_PVCs). 50 RWX PVCs = 50 extra Ganesha Deployments + 50 Services + 50 backing RWO PVCs
- Each Ganesha pod has a full container footprint (~50 MiB RSS minimum)
- Operationally noisy: `kubectl get pods -n app` shows two pods per RWX PVC (user app + NFS server)

Goal of this rewrite:

- **One NFS server DaemonSet pod per node**, serving all RWX PVCs whose backing LV is on that node
- Pod count is O(N_nodes), not O(N_PVCs)
- Match Piraeus's `linstor-csi-nfs-server` shape: privileged containers, live export reload via DBus, driven by a reactor-style config mechanism

---

## Trade-offs accepted

This design was selected after explicitly rejecting alternatives:

| Option | Pod count | Disrupts add-PVC? | Privileged? | Chosen? |
|---|---|---|---|---|
| Current (per-PVC Deployment) | O(N_PVCs) | only that one PVC | no | — |
| **DaemonSet + privileged + live reload (Piraeus-style)** | O(N_nodes) | zero (live) | **yes** | ✅ |
| DaemonSet + unprivileged + rolling restart | O(N_nodes) | all PVCs on the node | no | no — worse UX |
| Controller-managed per-node pod + Service swap | O(N_nodes) | all PVCs on the node | no | no — complex |

**Explicitly accepted:**

1. **Privileged NFS server pods** — The DaemonSet pods run `privileged: true` with hostPath bind mounts into `/dev` and `/sys`, matching Piraeus's pattern. This is required because:
    - The pod mounts per-PVC LVM volumes inside its own mount namespace (no K8s CSI layer)
    - Dynamic volume mount/unmount requires `CAP_SYS_ADMIN` plus raw device access
    - Pod security policies that forbid privileged will reject this DaemonSet — operators targeting hardened clusters need to weigh TopoLVM RWX against PSS `restricted`.
2. **No HA improvement over today.** TopoLVM LVs are local, not replicated. If a node dies, every RWX PVC whose LV was on that node is unavailable until the node comes back. The DaemonSet shape doesn't change that — it matches Piraeus's shape but not its replication semantics. The win here is pod-count, not availability.
3. **Per-PVC downtime during migration.** West is already running the old per-PVC design. During migration, each PVC will see a small (seconds) NFS outage while its client remounts from the old pod's Service IP to the DaemonSet pod's Service IP.

---

## Architecture

### Components

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           control plane                                  │
│                                                                          │
│  ┌────────────────────────────┐                                          │
│  │   rwx-pvc-controller       │   (existing — rewritten)                 │
│  │   watches: RWX PVCs        │                                          │
│  │   creates: backing PVC,    │                                          │
│  │            ConfigMap entry,│                                          │
│  │            mirror PV       │                                          │
│  └─────────────┬──────────────┘                                          │
│                │                                                         │
│                │ updates                                                 │
│                ▼                                                         │
│  ┌────────────────────────────┐                                          │
│  │ RWXExports ConfigMap       │   (cluster-scoped, one per cluster)      │
│  │  entries:                  │                                          │
│  │   - pvc: <ns>/<name>       │                                          │
│  │     backing-pvc-uid: ...   │                                          │
│  │     export-path: /exports/ │                                          │
│  │       <ns>/<name>          │                                          │
│  │     host-node: <nodename>  │                                          │
│  └─────────────┬──────────────┘                                          │
│                │ mounted into                                            │
└────────────────┼─────────────────────────────────────────────────────────┘
                 ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                   node N (every node has one)                            │
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────┐      │
│  │  topolvm-rwx-nfs DaemonSet pod (privileged)                    │      │
│  │  ┌──────────────────────┐     ┌──────────────────────────────┐ │      │
│  │  │ reactor sidecar      │     │  ganesha.nfsd (main)         │ │      │
│  │  │  - watches ConfigMap │◀───▶│  - listens on 2049           │ │      │
│  │  │  - filters by node   │DBus │  - reloads exports on demand │ │      │
│  │  │  - mounts LVs into   │     │  - reads /exports/<ns>/<name>│ │      │
│  │  │    /exports/*        │     │    that reactor has mounted  │ │      │
│  │  │  - dbus AddExport    │     └──────────────────────────────┘ │      │
│  │  └──────────────────────┘                                      │      │
│  │  hostPath: /dev, /sys                                          │      │
│  │  privileged: true, CAP_SYS_ADMIN                               │      │
│  └────────────────────────────────────────────────────────────────┘      │
│                          ▲                                               │
│                          │ reads LVs via host /dev/nvme-pool/<lv>        │
│                          │                                               │
│          ┌───────────────┴─────────────┐                                 │
│          │  TopoLVM lvmd (existing)    │                                 │
│          │  creates LVs for backing    │                                 │
│          │  RWO PVCs on this node      │                                 │
│          └─────────────────────────────┘                                 │
└─────────────────────────────────────────────────────────────────────────┘
                          │
                          ▼ NFS over ClusterIP
            ┌──────────────────────────────┐
            │   user pods on any node      │
            │   mount RWX PVC              │
            └──────────────────────────────┘
```

### Service model

Each RWX PVC still gets:

- A **backing RWO PVC** (same as today) — this is what TopoLVM lvmd carves into an LV. Lives on whichever node the PVC's scheduler picked.
- A **ClusterIP Service** — points at the DaemonSet pod **on the node hosting the backing LV** (via endpoint slice with a single endpoint selector matching `topology.kubernetes.io/node=<hostnode>`).
- A **mirror PV** (same as today) — `CSI.Driver = nfs.csi.k8s.io`, `server: <svc-ip>`, `share: /exports/<ns>/<name>`.

The **DaemonSet** itself is one cluster-wide object. One pod per node. Each pod ignores RWX PVCs that don't live on its node.

---

## Component responsibilities

### 1. Rewritten `rwx-pvc-controller`

Same trigger conditions as today — watches PVCs using the `topolvm-provisioner-rwx` StorageClass. But its output is different:

**What it does NOT create anymore:**
- Per-PVC Ganesha Deployment
- Per-PVC ConfigMap of Ganesha config
- Per-PVC ServiceAccount

**What it does create/reconcile:**
- Backing RWO PVC (same as today)
- Entry in the shared `RWXExports` ConfigMap (see schema below)
- A Service pointing at the DaemonSet pod on the hosting node
- The mirror PV

**Feature flag:** `--rwx-mode=per-pvc|daemonset` on the controller binary. Default `per-pvc` during migration window. Flip to `daemonset` after west migration is complete.

### 2. New `RWXExports` ConfigMap

- **Name:** `topolvm-rwx-exports`
- **Namespace:** same as `topolvm-controller` (`operator-storage`)
- **Format:** single key `exports.yaml` containing a YAML document:

```yaml
exports:
  - pvc:
      namespace: app
      name: data
    uid: a1b2c3d4-...              # PVC UID for idempotent handling
    backing:
      pvcName: data-rwx-backing
      lvName: pvc-a1b2c3d4-...     # the LV name as known to lvmd
      volumeGroup: nvme-pool
      thinPool: thin-pool
    hostNode: dxrhhq2              # which node's LV backs this
    exportPath: /exports/app/data  # where reactor will mount the LV
    fsType: xfs
    readOnly: false
  - pvc:
      namespace: other
      name: logs
    ...
```

- Updates propagate to each DaemonSet pod via the Kubelet's ConfigMap-to-file sync (typically sub-minute).

### 3. New DaemonSet `topolvm-rwx-nfs`

Runs one pod per node with:

- **2 containers**: `reactor` (sidecar) + `ganesha` (main)
- **Privileged: true**, `hostPID: true`, `hostIPC: false`
- hostPath volumes:
    - `/dev` → `/host/dev` (read-write, required for device node access)
    - `/sys` → `/sys` (read-only)
    - `/run` → `/run`
- ConfigMap volume:
    - `topolvm-rwx-exports` → `/etc/topolvm-rwx/exports.yaml`
- EmptyDir volume for coordination:
    - `/exports` — the reactor mounts LVs here; Ganesha reads from here
    - `/var/run/dbus` — dbus socket between reactor and ganesha
- NodeSelector: every node that runs lvmd (via label `topolvm.io/lvmd=true`, which the chart already emits)

### 4. New `reactor` sidecar

Custom Go program in `internal/rwx_daemon/`. Per node it:

1. Reads `NODE_NAME` from downward API
2. Watches `/etc/topolvm-rwx/exports.yaml`
3. Filters entries to `.hostNode == $NODE_NAME`
4. For each entry:
    - Ensures the LV is activated (`lvchange -ay <vg>/<lv>` — idempotent)
    - Ensures the filesystem is mounted at `/exports/<ns>/<name>`
    - Ensures Ganesha knows about the export via DBus `AddExport`
5. For each entry that was previously-present-but-now-gone:
    - DBus `RemoveExport` on Ganesha
    - Unmount from `/exports/<ns>/<name>`
    - Optionally `lvchange -an` (but we typically leave the LV active since lvmd may also need it)

**Why not just shell out to a script?** Because the reactor needs:
- To hold a DBus connection to Ganesha (persistent)
- To reconcile declaratively (safe to re-run)
- To log/emit metrics about export state

A Go binary ~300 lines is cheaper to maintain than equivalent shell, and it fits in the same image build we already have for `hypertopolvm`.

### 5. Ganesha main container

Same Ganesha binary as today, but:

- **Config** is minimal: just `NFS_CORE_PARAM`, `NFSv4`, and `DBUS { Enable = YES; }` so the reactor can register exports dynamically
- **No static exports in the config** — reactor adds them at runtime
- **`/var/lib/nfs/ganesha`** is an emptyDir per DaemonSet pod (state rebuilt on restart)

---

## Migration from per-PVC to DaemonSet

### Phase 1 — Ship dual-mode binary (1-2 weeks)

Both code paths live in the binary. Feature flag `--rwx-mode=per-pvc|daemonset` (default `per-pvc`).

- Old `internal/nfs/` and `internal/controller/rwx_pvc_controller.go` unchanged in the `per-pvc` code path
- New `internal/rwx_daemon/` package with the reactor + DaemonSet manifest builder
- New `internal/controller/rwx_pvc_controller_daemonset.go` with the new reconciler
- At startup, controller picks which reconciler to wire based on the flag

### Phase 2 — Test on central (0.5 week)

Central today has `16.0.1-rwx.1` (old design) running. We:

1. Cut a new image tag with the dual-mode binary, e.g. `v16.1.0-rwx-daemon.1`
2. On central, `helm upgrade` the controller with `--rwx-mode=daemonset`
3. Delete the one RWX PVC we used for smoke-testing
4. Create a new RWX PVC — this time it flows through the new code path
5. Verify cross-node RWX works with the DaemonSet pod serving

No live user workload on central today — safe playground.

### Phase 3 — Migrate west (production) (1-2 weeks)

West has live RWX PVCs. Cut over one PVC at a time:

1. `helm upgrade` the topolvm controller to the dual-mode image, `--rwx-mode=per-pvc` (default — no behavior change yet)
2. Deploy the DaemonSet alongside, running but with an empty ConfigMap (all PVCs still served by old Deployments — DaemonSet is idle)
3. Per PVC, to migrate:
    - Add the PVC to the ConfigMap (reactor on the hosting node picks it up, starts serving it)
    - Delete the per-PVC Deployment
    - Swap the PV's Service reference from old per-PVC Service to the DaemonSet-backed Service
    - User pods transparently reconnect via NFS (brief seconds-scale stale-handle until remount)
4. After all PVCs migrated, flip the controller's `--rwx-mode=daemonset` so no new PVCs use the old code path
5. Remove old code path in a follow-up release

### Phase 4 — Remove old code path (1 release cycle later)

Once all known clusters are on `daemonset` mode for at least one release, delete the `per-pvc` branch from the codebase. Mark CSI chart values like `controller.rwx.enabled` and `topolvm.io/backing-storage-class` as still-honored-but-migrated.

---

## Failure modes

### Node failure

- DaemonSet pod on the dead node is unreachable
- Every RWX PVC whose `hostNode` was that dead node becomes unavailable
- Same as today for the non-RWX PVCs (LVM is node-local)
- When node comes back, DaemonSet pod re-runs reactor, reactivates LVs, re-registers exports. No controller intervention needed.

### DaemonSet pod OOM / crash

- Kubelet restarts the pod
- Reactor on new pod reads the ConfigMap, remounts LVs, re-registers exports with the new Ganesha process
- NFS clients see stale file handles briefly, remount restores access
- Recovery time: seconds to ~30s

### ConfigMap update race

Two controllers update the ConfigMap at once (e.g. two RWX PVCs created simultaneously). Resolved via:

- Optimistic concurrency on ConfigMap `resourceVersion`
- Controller uses `CreateOrUpdate` with a retry-on-conflict loop

### LV not found on host

- Reactor logs the error, skips that export, moves on
- Export goes Degraded (we set a `RWXExport` CR condition if we add one — see "Open questions" below)
- User's pod hangs on the mount — same failure mode as today when a backing PVC doesn't exist

### ConfigMap size limit

Kubernetes ConfigMap limit is 1 MiB. Per-export entry is roughly 300 bytes. 1 MiB / 300 B ≈ 3,000 exports per cluster. Very unlikely to hit; if we do, switch to a dedicated CR (`RWXExportList` cluster-scoped) instead of a ConfigMap.

---

## Security impact

Compared to current design:

| Surface | Current (per-PVC) | DaemonSet |
|---|---|---|
| Privileged | No | **Yes** — DaemonSet pods |
| hostPath mounts | None | `/dev` read-write, `/sys` read-only |
| Host PID namespace | No | `hostPID: true` (required for dbus-daemon process visibility) |
| Pods that can compromise the node | User workloads (normal) | **DaemonSet pods now also can** |
| Capabilities | `SYS_ADMIN` narrow | `SYS_ADMIN` broad + full privileged |
| PSS compatibility | `baseline` or `restricted` OK | Requires `privileged` |

If operators target `PodSecurity: restricted` namespaces for TopoLVM, **this design breaks that**. Document as a deployment requirement.

---

## Test plan

### Unit tests (~3 days)

- `internal/rwx_daemon/config_test.go` — ConfigMap YAML round-trip
- `internal/rwx_daemon/reactor_test.go` — reconciliation loop with a fake mount/dbus client; cover: add, remove, update, restart, error retry
- `internal/controller/rwx_pvc_controller_daemonset_test.go` — reconciler creates the right objects; handles scheduling races
- Existing tests for per-PVC code path must continue to pass unmodified during the transition

### e2e (existing `rwx_test.go`) — **run in both modes**

The existing e2e at [test/e2e/rwx_test.go](../test/e2e/rwx_test.go) exercises the full cross-node RWX flow. Adapt it to:

- `RWX_MODE=per-pvc` — today's default, unchanged
- `RWX_MODE=daemonset` — new code path

Add to CI matrix so every PR runs both. Expect ~20 min for each leg.

### New e2e scenarios for DaemonSet mode

- **2 RWX PVCs on the same node, both served by one DaemonSet pod** — verify exports don't collide
- **Create + delete + create same PVC name** — verify reactor cleans up the old export before the new one lands
- **DaemonSet pod kill** — `kubectl delete pod -n operator-storage topolvm-rwx-nfs-xxxxx` while an RWX PVC has an active client; verify client recovers within 60s
- **Node cordoned during RWX use** — scheduler should route new RWX PVCs to other nodes (extension: is that what we want?)

### Central cluster smoke test (same as today)

After phase 2 cutover: single-PVC, two-pods-different-nodes, shared-write — already a one-liner.

---

## Work breakdown estimate

| Work | Estimate |
|---|---|
| New `internal/rwx_daemon/` package (reactor + config + dbus glue) | 4 days |
| New `internal/controller/rwx_pvc_controller_daemonset.go` | 2 days |
| Wire `--rwx-mode` flag, dual-path dispatch in binary | 0.5 day |
| DaemonSet + Ganesha config + minimal container image | 1 day |
| Helm chart updates: values, DaemonSet template, RBAC, new ServiceAccount | 1 day |
| Unit tests | 3 days |
| e2e adaptations (run existing in both modes) | 1 day |
| New e2e scenarios for DaemonSet | 2 days |
| Docs (user-facing + ops runbook) | 1 day |
| Central smoke + phase-2 rollout | 0.5 day |
| West migration (per-PVC rollover) | 2-3 days spread over a week |
| **Total** | **~3 weeks of focused work** |

---

## Open questions (need answers before implementation starts)

1. **Reactor image** — Should the reactor ship in the existing `hypertopolvm` image (making a new Go binary part of it) or as a separate image? Current recommendation: same binary as `hypertopolvm`, with `--role=rwx-reactor` dispatch. Keeps image count low.

2. **Ganesha image** — Today uses `ghcr.io/kubernetes-sigs/nfs-ganesha:V6.5` (see [constants.go:131](../constants.go#L131)). Does this image support DBus? If not, we need to either build our own or find one that does. Needs a verification step before coding.

3. **Scheduling of RWX PVCs to "best" nodes** — The backing RWO PVC goes to whichever node lvmd picks (usually via `topology.kubernetes.io/node` affinity). That becomes the `hostNode` in the export. Do we want any preference (e.g. anti-affinity — spread RWX across nodes)? Default: no special logic, same as today.

4. **Observability** — Today each per-PVC Ganesha is a discrete pod with discrete logs. With a DaemonSet, all exports' logs are interleaved in one pod's log stream. Do we want structured per-export log prefixes? Prometheus metrics for `rwx_export_bytes_read_per_pvc`? This is not blocking but worth deciding up front since retrofitting later is painful.

5. **Do we implement a `RWXExport` CR?** Right now the ConfigMap is the only surface. A CR would give `kubectl get rwxexport` semantics for users to see which PVCs are being served where. Adds complexity. My recommendation: **no, ship with ConfigMap only**, add a CR if operators ask for it.

6. **Phase 3 migration — who triggers per-PVC cutover?** An operator script, the controller itself (auto-migrate as PVCs' pods restart), or a manual `kubectl annotate pvc --rwx-mode=daemonset` per PVC? My recommendation: **controller auto-migrates on PVC reconcile** when `--rwx-mode=daemonset`, skipping the per-PVC flag. Simplest.

---

## Rollout decision matrix

Before merging any code for this rewrite, confirm:

- [ ] Privileged DaemonSet is acceptable for every cluster this image might ship to
- [ ] No cluster needs `PodSecurity: restricted` for the RWX DaemonSet namespace
- [ ] West migration is scheduled in a change window with ops availability
- [ ] CI has budget for a new ~20-min e2e matrix leg (or we accept longer PR times)

If any of those is "no", stop and revise before writing the code.
