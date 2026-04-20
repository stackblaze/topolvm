package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// S3Spec describes the remote S3-compatible object store used as the restic repository backend.
type S3Spec struct {
	// Endpoint is the S3 endpoint URL, e.g. "https://s3.us-east-1.amazonaws.com"
	// or a MinIO endpoint. Required.
	Endpoint string `json:"endpoint"`

	// Bucket is the target bucket name. One restic repository per PVC is
	// created under the key prefix "<namespace>/<pvc-name>/".
	Bucket string `json:"bucket"`

	// Region is the S3 region. Optional for providers that do not require it.
	//+kubebuilder:validation:Optional
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef references a Secret containing AWS_ACCESS_KEY_ID and
	// AWS_SECRET_ACCESS_KEY keys. The secret must live in the same namespace as
	// the topolvm-controller.
	CredentialsSecretRef corev1.SecretReference `json:"credentialsSecretRef"`

	// InsecureTLS disables TLS verification against the S3 endpoint. Use only
	// for self-signed endpoints during testing.
	//+kubebuilder:validation:Optional
	InsecureTLS bool `json:"insecureTLS,omitempty"`

	// Prefix is an optional key prefix prepended under the bucket root before
	// the per-PVC "<namespace>/<pvc>" path. Useful for isolating CI runs that
	// share a bucket, e.g. prefix="ci/<run-id>".
	//+kubebuilder:validation:Optional
	Prefix string `json:"prefix,omitempty"`
}

// RetentionSpec mirrors the restic forget --keep-* flags.
type RetentionSpec struct {
	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepLast int32 `json:"keepLast,omitempty"`

	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepHourly int32 `json:"keepHourly,omitempty"`

	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepDaily int32 `json:"keepDaily,omitempty"`

	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepWeekly int32 `json:"keepWeekly,omitempty"`

	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepMonthly int32 `json:"keepMonthly,omitempty"`

	//+kubebuilder:validation:Minimum=0
	//+kubebuilder:validation:Optional
	KeepYearly int32 `json:"keepYearly,omitempty"`
}

// BackupConfigSpec defines the cluster-wide backup configuration. A single
// BackupConfig named "default" is expected; additional BackupConfigs are
// ignored by the controller.
type BackupConfigSpec struct {
	// S3 is the remote object store configuration.
	S3 S3Spec `json:"s3"`

	// ResticPasswordSecretRef references a Secret with a single "password" key
	// used as the restic repository password. Losing this secret makes existing
	// backups unreadable.
	ResticPasswordSecretRef corev1.SecretReference `json:"resticPasswordSecretRef"`

	// Schedule is the default cron schedule applied to every backed-up PVC.
	// When empty, no backup CronJobs are created unless a PVC sets its own
	// schedule via the "topolvm.io/backup-schedule" annotation.
	//+kubebuilder:validation:Optional
	Schedule string `json:"schedule,omitempty"`

	// Retention is the default retention policy. Per-PVC annotations of the
	// form "topolvm.io/backup-retention-<bucket>" override individual buckets.
	//+kubebuilder:validation:Optional
	Retention RetentionSpec `json:"retention,omitempty"`

	// Selector restricts which PVCs are backed up. An empty selector matches
	// every PVC bound to a TopoLVM StorageClass. PVCs annotated
	// "topolvm.io/backup=false" are always skipped.
	//+kubebuilder:validation:Optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// KeepSnapshotAfterBackup controls whether the VolumeSnapshot created for
	// the backup is preserved after restic completes. When false (default) the
	// snapshot is deleted once the backup finishes, regardless of outcome.
	// Can be overridden per-PVC via "topolvm.io/backup-keep-snapshot".
	//+kubebuilder:default=false
	KeepSnapshotAfterBackup bool `json:"keepSnapshotAfterBackup,omitempty"`

	// VolumeSnapshotClassName is the VolumeSnapshotClass used to snapshot
	// PVCs prior to backup. If empty the controller falls back to the default
	// VolumeSnapshotClass for the topolvm driver.
	//+kubebuilder:validation:Optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`

	// Suspend pauses creation of new backup Jobs without deleting existing
	// CronJobs or previously taken backups.
	//+kubebuilder:validation:Optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupConfigStatus reports the observed state of the backup fleet.
type BackupConfigStatus struct {
	// ObservedGeneration matches BackupConfig.metadata.generation when the
	// status was last updated.
	//+kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ManagedPVCs is the number of PVCs the controller currently has a
	// backup CronJob for.
	//+kubebuilder:validation:Optional
	ManagedPVCs int32 `json:"managedPVCs,omitempty"`

	// Conditions is the list of conditions for this BackupConfig. Typical
	// condition types: "Ready", "CredentialsValid".
	//+kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// BackupConfig is the cluster-scoped configuration for TopoLVM backup/restore
// via restic.
type BackupConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupConfigSpec   `json:"spec,omitempty"`
	Status BackupConfigStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// BackupConfigList contains a list of BackupConfig.
type BackupConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupConfig{}, &BackupConfigList{})
}
