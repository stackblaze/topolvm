# Backup and Restore (restic)

TopoLVM ships an opt-in controller that backs up PVCs to an S3-compatible
object store using [restic](https://restic.net/) and restores them back into
fresh PVCs. The model is inspired by
[VolSync](https://volsync.readthedocs.io/): each backup run snapshots the
source PVC, provisions a read-only temp PVC from the snapshot, runs a restic
mover Job against it, and then tears the transient objects down.

One restic repository is created **per source PVC** inside the configured
bucket, keyed by `<namespace>/<pvc-name>` (optionally with a cluster-wide
prefix for shared buckets).

## Enabling the feature

In your Helm values:

```yaml
controller:
  backup:
    enabled: true
    # resticImage: restic/restic:0.17.3   # override if needed
    # moverServiceAccount: default         # SA used by mover/restore Jobs
```

Then create the two Secrets in the controller's namespace
(`topolvm-system` by default) and apply a `BackupConfig`.

## `BackupConfig` (cluster-scoped, singleton)

Only the `BackupConfig` named `default` is honored; additional objects are
ignored.

```yaml
apiVersion: topolvm.io/v1
kind: BackupConfig
metadata:
  name: default
spec:
  s3:
    endpoint: https://s3.example.com
    bucket: topolvm-backups
    region: us-east-1                 # optional
    # prefix: shared/tenant-a         # optional, inserted between bucket and <ns>/<pvc>
    credentialsSecretRef:
      name: s3-creds
      namespace: topolvm-system       # default: controller namespace
  resticPasswordSecretRef:
    name: restic-pw
    namespace: topolvm-system
  schedule: "0 2 * * *"               # global default; empty disables backups
  retention:
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 12
  volumeSnapshotClassName: topolvm-provisioner-thin
  keepSnapshotAfterBackup: false      # delete the VolumeSnapshot after backup
  selector:                           # optional; empty matches every TopoLVM PVC
    matchLabels:
      backup: "yes"
  suspend: false
```

Required Secrets:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3-creds
  namespace: topolvm-system
stringData:
  AWS_ACCESS_KEY_ID: ...
  AWS_SECRET_ACCESS_KEY: ...
---
apiVersion: v1
kind: Secret
metadata:
  name: restic-pw
  namespace: topolvm-system
stringData:
  password: <repository password>
```

The BackupConfig controller mirrors these Secrets into every namespace that
contains a managed PVC.

## Per-PVC overrides (annotations)

Put these on the source `PersistentVolumeClaim`:

| Annotation | Meaning |
| --- | --- |
| `topolvm.io/backup=false` | Skip this PVC entirely. |
| `topolvm.io/backup-schedule=<cron>` | Override global schedule. |
| `topolvm.io/backup-keep-snapshot=true` | Keep the VolumeSnapshot around after each run. |
| `topolvm.io/backup-retention-last=<n>` | Override global `keepLast`. |
| `topolvm.io/backup-retention-hourly=<n>` | Override `keepHourly`. |
| `topolvm.io/backup-retention-daily=<n>` | Override `keepDaily`. |
| `topolvm.io/backup-retention-weekly=<n>` | Override `keepWeekly`. |
| `topolvm.io/backup-retention-monthly=<n>` | Override `keepMonthly`. |
| `topolvm.io/backup-retention-yearly=<n>` | Override `keepYearly`. |

## `PVCBackup` (namespaced, per PVC)

The BackupConfig controller maintains one `PVCBackup` per matching PVC,
named after the PVC. Inspect:

```bash
kubectl get pvcbackup -A
```

`.status.phase` walks through `Idle → Snapshotting → Cloning → Moving →
Cleanup → Synced|Failed`. A failed run sets `Failed` and waits for the next
fire time; it does not block future runs.

To force an immediate run outside the schedule (e.g. before maintenance):

```bash
kubectl patch pvcbackup -n app data --type=merge \
  -p '{"spec":{"trigger":"manual-'"$(date +%s)"'"}}'
```

Users may also author their own `PVCBackup` objects directly — the
BackupConfig controller never modifies objects it did not create (it keys
on the `topolvm.io/backup-managed-by` label).

## `Restore` (namespaced)

A `Restore` CR creates a fresh PVC in its own namespace and populates it
from a restic snapshot.

```yaml
apiVersion: topolvm.io/v1
kind: Restore
metadata:
  name: data-restore
  namespace: app
spec:
  sourceNamespace: app          # namespace of the original PVC
  sourcePVCName: data           # original PVC name (used as the repo key)
  snapshotID: ""                # empty = "latest"
  targetPVCName: data-restored  # new PVC created in this namespace
  size: 10Gi
  storageClassName: topolvm-provisioner-thin
```

The Restore target PVC is **not** owner-referenced by the Restore CR:
deleting the Restore does not delete the restored data.

## Lifecycle details

- **Snapshot mode** — VolumeSnapshots are used regardless of thin/thick
  provisioning. Thick snapshots consume full size on the VG; factor that
  into capacity planning.
- **Backup atomicity** — each run uses a dedicated VolumeSnapshot and temp
  PVC (names include a `runID` suffix) so interrupted runs never collide
  with the next schedule slot.
- **Cleanup guarantees** — the temp PVC is always deleted after the mover
  Job completes. The VolumeSnapshot is also deleted unless
  `keepSnapshotAfterBackup=true`. All three child objects carry owner
  references to the `PVCBackup`, so deleting the `PVCBackup` cascades
  cleanup via standard Kubernetes GC.
- **Restart safety** — phase transitions are recorded on the CR's status
  subresource, so the controller can resume mid-run after a restart
  without re-taking snapshots or re-running backups already in flight.

## Operational tips

- Give the mover Job a dedicated ServiceAccount per namespace if your
  SecurityContext / PodSecurity policy needs a non-default SA.
- Monitor `PVCBackup.status.phase` + `.status.lastSyncTime` via
  Prometheus/alerting for any PVC where `Failed` persists across multiple
  fire times.
- `restic -r <repo> snapshots` against the bucket from any machine with
  the restic password verifies the backup data is usable out-of-band.
