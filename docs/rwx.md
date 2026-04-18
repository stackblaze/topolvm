# ReadWriteMany (RWX) Support

TopoLVM volumes are node-local LVM logical volumes, so the CSI driver only
supports `ReadWriteOnce` natively. To offer `ReadWriteMany` to workloads, TopoLVM
can layer NFS (via [NFS-Ganesha][ganesha]) on top of a conventional RWO TopoLVM
volume. This page describes how the feature works, how to enable it, and the
trade-offs to expect.

[ganesha]: https://github.com/nfs-ganesha/nfs-ganesha

## How it works

When an RWX PVC is created, an in-controller reconciler provisions:

1. **A backing RWO PVC** bound to a normal TopoLVM StorageClass. This is where
   the actual data lives. TopoLVM provisions it on one node as usual.
2. **A per-volume Ganesha Deployment** (1 replica, pinned to the node holding
   the backing PVC). Ganesha mounts the backing PVC at `/export` and exposes it
   over NFSv4.
3. **A ClusterIP Service** in front of the Deployment (ports 2049, 20048, 111).
4. **A static NFS PersistentVolume** that references the Service and is bound
   to the user's RWX PVC. The PV uses the
   [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) driver,
   which must be installed in the cluster.

Consumer pods on any node mount the NFS PV and get true RWX semantics; the
backing PVC is never mounted directly by anything except the Ganesha pod.

## Prerequisites

- TopoLVM installed and working for RWO.
- [csi-driver-nfs][nfs] installed; TopoLVM does not bundle it. The version
  shipped upstream (`nfs.csi.k8s.io`) is the driver the static PV points at.
- A TopoLVM StorageClass you are willing to use for the RWO backing volume
  (any standard `topolvm-provisioner` class works).

[nfs]: https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/install-csi-driver.md

## Enabling the feature

Set `controller.rwx.enabled=true` in Helm:

```yaml
controller:
  rwx:
    enabled: true
    # ganeshaImage: my.registry/ganesha:hardened  # optional override
```

Then create a StorageClass that marks itself as RWX:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: topolvm-rwx
provisioner: topolvm.io
parameters:
  # Tells the TopoLVM controller to treat PVCs of this class as RWX requests.
  topolvm.io/access-mode: rwx
  # The RWO TopoLVM StorageClass used to provision the backing volume.
  topolvm.io/backing-storage-class: topolvm-provisioner
  # Optional: passed through to the backing PVC.
  csi.storage.k8s.io/fstype: ext4
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
allowVolumeExpansion: true
```

> **Note:** The RWX StorageClass itself does *not* serve as the provisioner for
> the user PVC — the user PVC ends up bound to an NFS PV via csi-driver-nfs.
> The StorageClass is only the marker the reconciler watches for.

## Example usage

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-data
spec:
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 5Gi
  storageClassName: topolvm-rwx
```

Two pods in the same namespace can now mount `shared-data` simultaneously, on
different nodes.

## Snapshots

RWX PVCs support snapshots via the same `VolumeSnapshot` API used for RWO
volumes. Under the hood the controller creates a mirror `VolumeSnapshot`
against the backing RWO PVC, letting TopoLVM's native (thin-pool) snapshot path
do the actual work. Requirements:

- The backing TopoLVM StorageClass must use a **thin** device class (snapshots
  are not supported on thick provisioning — see
  [docs/snapshot-and-restore.md](./snapshot-and-restore.md)).
- The `VolumeSnapshotClass` you use must target `topolvm.io` (the backing PVC's
  driver), not `nfs.csi.k8s.io`.

Example:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: shared-data-snap-1
spec:
  volumeSnapshotClassName: topolvm-snapshot   # targets topolvm.io
  source:
    persistentVolumeClaimName: shared-data    # the RWX PVC
```

Restoring a snapshot into a new RWX PVC works the same way — the reconciler
rewrites the `dataSource` so TopoLVM clones from the mirror snapshot:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-data-restored
spec:
  accessModes: [ReadWriteMany]
  storageClassName: topolvm-rwx
  dataSource:
    name: shared-data-snap-1
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
  resources:
    requests:
      storage: 5Gi
```

**Consistency:** LVM thin snapshots are crash-consistent, not
application-consistent. Quiesce your workload (or use application-level
snapshots) if you need guarantees beyond "power was cut."

## Resize

RWX PVCs support online expansion. Set `allowVolumeExpansion: true` on the RWX
StorageClass and edit the PVC's `spec.resources.requests.storage` — the
controller grows the backing PVC (TopoLVM resizes the LV and filesystem) and
updates the NFS PV capacity to match. NFSv4 clients see the new size without
remounting.

Shrinks are not supported (CSI does not support them; requests to reduce size
are ignored).

## Limitations

- **Single Ganesha replica.** If the node running the Ganesha pod is drained,
  NFS clients pause until the pod is rescheduled and the backing PVC is
  reattached. This is usually seconds; hard crashes can be longer. NFSv4 hard
  mounts retry transparently.
- **No per-export access control.** Exports are visible to any pod in the
  cluster that can reach the Service. Network policies are the right tool if
  you need restriction.
- **Reduced but non-zero privileges.** The Ganesha pod runs without the full
  `privileged: true` bit, but it does require a set of Linux capabilities
  (`SYS_ADMIN`, `DAC_READ_SEARCH`, `CHOWN`, `FOWNER`, `SETUID`, `SETGID`).
  Clusters enforcing the `restricted` Pod Security Standard will need a
  privileged profile for the PVC's namespace.

## Troubleshooting

- **Pods stuck in `ContainerCreating` with `MountVolume.SetUp failed`.** Verify
  csi-driver-nfs is installed and healthy. Check the NFS PV's `server` attribute
  points at a reachable Service ClusterIP.
- **`PVC is Pending` with no events.** Check the topolvm-controller logs for
  errors about the backing StorageClass — the most common cause is forgetting
  `topolvm.io/backing-storage-class` on the RWX StorageClass.
- **Ganesha pod in `CrashLoopBackOff`.** The image requires privileged mode.
  Confirm your cluster's Pod Security admission policy allows privileged pods
  in the PVC's namespace, or run the controller in a cluster profile that
  permits it.
