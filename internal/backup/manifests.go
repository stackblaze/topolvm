package backup

import (
	"fmt"
	"strings"

	"github.com/topolvm/topolvm"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// RuntimeConfig is the merged view the reconciler passes to each manifest
// helper: a PVCBackup's effective spec combined with cluster-wide
// BackupConfig settings and the resolved source PVC's storage class / size.
type RuntimeConfig struct {
	// Source PVC identity.
	PVCNamespace string
	PVCName      string
	PVCSize      resource.Quantity

	// Storage class of the source PVC, copied onto the temp PVC.
	StorageClassName string

	// Parent PVCBackup (used for owner references and child naming).
	PVCBackupName string
	PVCBackupUID  string

	// Effective snapshot/retention/keep settings for this run.
	VolumeSnapshotClassName string
	Retention               topolvmv1.RetentionSpec
	KeepSnapshotAfterBackup bool

	// S3 repo settings (from BackupConfig).
	S3Endpoint    string
	S3Bucket      string
	S3Region      string
	S3InsecureTLS bool
	S3Prefix      string

	// Secret names (must exist in the PVC's namespace; the BackupConfig
	// controller is responsible for propagating them there if needed).
	S3CredentialsSecretName  string
	ResticPasswordSecretName string

	// Mover image and SA.
	ResticImage        string
	ServiceAccountName string
}

// SnapshotName, TempPVCName, and MoverJobName are deterministic per
// PVCBackup run. The runID is typically the first 8 chars of the
// VolumeSnapshot.UID (or a reconcile-generated timestamp when taking the
// snapshot for the first time).
func SnapshotName(pvcName, runID string) string {
	return fmt.Sprintf("%s%s%s", pvcName, topolvm.BackupJobSnapshotInfix, runID)
}

func TempPVCName(pvcName, runID string) string {
	return fmt.Sprintf("%s%s%s", pvcName, topolvm.BackupJobTempPVCInfix, runID)
}

func MoverJobName(pvcName, runID string) string {
	return fmt.Sprintf("%s-backup-mover-%s", pvcName, runID)
}

func RestoreJobName(restoreName string) string {
	return restoreName + topolvm.RestoreJobSuffix
}

// RepoURL returns the restic repository URL for a PVC. One repo per PVC:
//
//	s3:<endpoint>/<bucket>/[<prefix>/]<namespace>/<pvc-name>
//
// Prefix may be empty and is used mainly to partition shared buckets for CI.
func RepoURL(endpoint, bucket, prefix, pvcNamespace, pvcName string) string {
	base := fmt.Sprintf("s3:%s/%s", strings.TrimSuffix(endpoint, "/"), bucket)
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		base = fmt.Sprintf("%s/%s", base, prefix)
	}
	return fmt.Sprintf("%s/%s/%s", base, pvcNamespace, pvcName)
}

func commonLabels(cfg RuntimeConfig) map[string]string {
	return map[string]string{
		topolvm.CreatedbyLabelKey:            topolvm.CreatedbyLabelValue,
		topolvm.BackupManagedByLabel:         topolvm.BackupManagedByValue,
		topolvm.BackupOwnerPVCNamespaceLabel: cfg.PVCNamespace,
		topolvm.BackupOwnerPVCNameLabel:      cfg.PVCName,
	}
}

func ownerRef(cfg RuntimeConfig) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         topolvmv1.GroupVersion.String(),
		Kind:               "PVCBackup",
		Name:               cfg.PVCBackupName,
		UID:                types.UID(cfg.PVCBackupUID),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// Snapshot returns a VolumeSnapshot for the source PVC, owned by the PVCBackup.
func Snapshot(cfg RuntimeConfig, runID string) *snapshotv1.VolumeSnapshot {
	var snapClass *string
	if cfg.VolumeSnapshotClassName != "" {
		c := cfg.VolumeSnapshotClassName
		snapClass = &c
	}
	pvcName := cfg.PVCName
	return &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            SnapshotName(cfg.PVCName, runID),
			Namespace:       cfg.PVCNamespace,
			Labels:          commonLabels(cfg),
			OwnerReferences: []metav1.OwnerReference{ownerRef(cfg)},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: snapClass,
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvcName,
			},
		},
	}
}

// TempPVC returns the read-only PVC provisioned from the VolumeSnapshot,
// owned by the PVCBackup.
func TempPVC(cfg RuntimeConfig, runID string) *corev1.PersistentVolumeClaim {
	apiGroup := "snapshot.storage.k8s.io"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            TempPVCName(cfg.PVCName, runID),
			Namespace:       cfg.PVCNamespace,
			Labels:          commonLabels(cfg),
			OwnerReferences: []metav1.OwnerReference{ownerRef(cfg)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(cfg.StorageClassName),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: cfg.PVCSize,
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     SnapshotName(cfg.PVCName, runID),
			},
		},
	}
}

// MoverJob returns the restic mover Job. The Job Pod mounts the already-bound
// temp PVC read-only at /data and runs restic backup + restic forget.
func MoverJob(cfg RuntimeConfig, runID string) *batchv1.Job {
	labels := commonLabels(cfg)
	env := moverEnv(cfg)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            MoverJobName(cfg.PVCName, runID),
			Namespace:       cfg.PVCNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef(cfg)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccountName,
					Containers: []corev1.Container{
						{
							Name:    "restic",
							Image:   cfg.ResticImage,
							Command: []string{"/bin/sh", "-c", backupScript},
							Env:     env,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: TempPVCName(cfg.PVCName, runID),
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}
}

func moverEnv(cfg RuntimeConfig) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "RESTIC_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.ResticPasswordSecretName},
					Key:                  topolvm.ResticPasswordSecretKey,
				},
			},
		},
		{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.S3CredentialsSecretName},
					Key:                  topolvm.S3AccessKeyIDSecretKey,
				},
			},
		},
		{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.S3CredentialsSecretName},
					Key:                  topolvm.S3SecretKeySecretKey,
				},
			},
		},
		{Name: "RESTIC_REPOSITORY", Value: RepoURL(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Prefix, cfg.PVCNamespace, cfg.PVCName)},
		{Name: "PVC_NAMESPACE", Value: cfg.PVCNamespace},
		{Name: "PVC_NAME", Value: cfg.PVCName},
		{Name: "KEEP_LAST", Value: intOrEmpty(cfg.Retention.KeepLast)},
		{Name: "KEEP_HOURLY", Value: intOrEmpty(cfg.Retention.KeepHourly)},
		{Name: "KEEP_DAILY", Value: intOrEmpty(cfg.Retention.KeepDaily)},
		{Name: "KEEP_WEEKLY", Value: intOrEmpty(cfg.Retention.KeepWeekly)},
		{Name: "KEEP_MONTHLY", Value: intOrEmpty(cfg.Retention.KeepMonthly)},
		{Name: "KEEP_YEARLY", Value: intOrEmpty(cfg.Retention.KeepYearly)},
	}
	if cfg.S3Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: cfg.S3Region})
	}
	return env
}

func intOrEmpty(n int32) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// backupScript runs inside the restic container. /data is the read-only
// temp PVC populated from the VolumeSnapshot. The mover never touches the
// snapshot or PVC lifecycle — the reconciler owns cleanup.
const backupScript = `#!/bin/sh
set -eu

echo "[restic] repository: ${RESTIC_REPOSITORY}"

if ! restic snapshots >/dev/null 2>&1; then
  echo "[restic] initializing repository"
  restic init
fi

echo "[restic] backing up /data"
restic backup /data \
  --tag "pvc=${PVC_NAMESPACE}/${PVC_NAME}" \
  --host "${PVC_NAMESPACE}"

FORGET_ARGS=""
[ -n "${KEEP_LAST:-}" ]    && FORGET_ARGS="${FORGET_ARGS} --keep-last ${KEEP_LAST}"
[ -n "${KEEP_HOURLY:-}" ]  && FORGET_ARGS="${FORGET_ARGS} --keep-hourly ${KEEP_HOURLY}"
[ -n "${KEEP_DAILY:-}" ]   && FORGET_ARGS="${FORGET_ARGS} --keep-daily ${KEEP_DAILY}"
[ -n "${KEEP_WEEKLY:-}" ]  && FORGET_ARGS="${FORGET_ARGS} --keep-weekly ${KEEP_WEEKLY}"
[ -n "${KEEP_MONTHLY:-}" ] && FORGET_ARGS="${FORGET_ARGS} --keep-monthly ${KEEP_MONTHLY}"
[ -n "${KEEP_YEARLY:-}" ]  && FORGET_ARGS="${FORGET_ARGS} --keep-yearly ${KEEP_YEARLY}"

if [ -n "${FORGET_ARGS}" ]; then
  echo "[restic] forget${FORGET_ARGS} --prune"
  # shellcheck disable=SC2086
  restic forget ${FORGET_ARGS} --host "${PVC_NAMESPACE}" --tag "pvc=${PVC_NAMESPACE}/${PVC_NAME}" --prune
fi

echo "[restic] done"
`

// RestoreConfig carries everything the Restore reconciler passes to the
// restore Job builder. Mirrors RuntimeConfig but for restore-only fields.
type RestoreConfig struct {
	// Restore CR identity (for naming + owner ref).
	RestoreName      string
	RestoreNamespace string
	RestoreUID       string

	// Target PVC in the Restore's namespace.
	TargetPVCName string

	// Source repo coordinates.
	SourceNamespace string
	SourcePVCName   string
	SnapshotID      string

	// S3 settings (from BackupConfig).
	S3Endpoint    string
	S3Bucket      string
	S3Region      string
	S3InsecureTLS bool
	S3Prefix      string

	S3CredentialsSecretName  string
	ResticPasswordSecretName string

	ResticImage        string
	ServiceAccountName string
}

func restoreOwnerRef(cfg RestoreConfig) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         topolvmv1.GroupVersion.String(),
		Kind:               "Restore",
		Name:               cfg.RestoreName,
		UID:                types.UID(cfg.RestoreUID),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// RestoreJob returns a one-shot Job that restores a restic snapshot into
// the target PVC. The Job is owned by the Restore CR so deletion of the
// Restore cascades to the Job (but not the target PVC — that is owned by
// the user / their namespace).
func RestoreJob(cfg RestoreConfig) *batchv1.Job {
	labels := map[string]string{
		topolvm.CreatedbyLabelKey:    topolvm.CreatedbyLabelValue,
		topolvm.BackupManagedByLabel: topolvm.BackupManagedByValue,
	}
	snapID := cfg.SnapshotID
	if snapID == "" {
		snapID = "latest"
	}
	env := []corev1.EnvVar{
		{
			Name: "RESTIC_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.ResticPasswordSecretName},
					Key:                  topolvm.ResticPasswordSecretKey,
				},
			},
		},
		{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.S3CredentialsSecretName},
					Key:                  topolvm.S3AccessKeyIDSecretKey,
				},
			},
		},
		{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.S3CredentialsSecretName},
					Key:                  topolvm.S3SecretKeySecretKey,
				},
			},
		},
		{Name: "RESTIC_REPOSITORY", Value: RepoURL(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Prefix, cfg.SourceNamespace, cfg.SourcePVCName)},
		{Name: "SOURCE_PVC_NAMESPACE", Value: cfg.SourceNamespace},
		{Name: "SOURCE_PVC_NAME", Value: cfg.SourcePVCName},
		{Name: "SNAPSHOT_ID", Value: snapID},
	}
	if cfg.S3Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: cfg.S3Region})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            RestoreJobName(cfg.RestoreName),
			Namespace:       cfg.RestoreNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{restoreOwnerRef(cfg)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccountName,
					Containers: []corev1.Container{
						{
							Name:    "restic",
							Image:   cfg.ResticImage,
							Command: []string{"/bin/sh", "-c", restoreScript},
							Env:     env,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: cfg.TargetPVCName,
								},
							},
						},
					},
				},
			},
		},
	}
}

const restoreScript = `#!/bin/sh
set -eu

echo "[restic] repository: ${RESTIC_REPOSITORY}"
echo "[restic] restoring snapshot ${SNAPSHOT_ID} into /data"

if [ -n "$(ls -A /data 2>/dev/null)" ]; then
  echo "[restic] /data is not empty; refusing to restore" >&2
  exit 2
fi

restic restore "${SNAPSHOT_ID}" \
  --target /data \
  --tag "pvc=${SOURCE_PVC_NAMESPACE}/${SOURCE_PVC_NAME}"

# restic restore --target /data places the original absolute path under /data
# (i.e. /data/data). Flatten so the PVC root mirrors the source.
if [ -d /data/data ]; then
  mv /data/data/* /data/ 2>/dev/null || true
  mv /data/data/.[!.]* /data/ 2>/dev/null || true
  rmdir /data/data
fi

echo "[restic] restore complete"
`
