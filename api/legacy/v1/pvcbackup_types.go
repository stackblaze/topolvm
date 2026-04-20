package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PVCBackupPhase is the high-level phase of a PVCBackup reconciliation.
type PVCBackupPhase string

const (
	// PVCBackupPhaseIdle means the controller is waiting for the next
	// scheduled fire time.
	PVCBackupPhaseIdle PVCBackupPhase = "Idle"

	// PVCBackupPhaseSnapshotting means a VolumeSnapshot has been created
	// and the controller is waiting for it to become ready.
	PVCBackupPhaseSnapshotting PVCBackupPhase = "Snapshotting"

	// PVCBackupPhaseCloning means a temp PVC is being provisioned from the
	// ready VolumeSnapshot and the controller is waiting for it to bind.
	PVCBackupPhaseCloning PVCBackupPhase = "Cloning"

	// PVCBackupPhaseMoving means the restic mover Job is running against
	// the temp PVC.
	PVCBackupPhaseMoving PVCBackupPhase = "Moving"

	// PVCBackupPhaseCleanup means the mover has finished and the
	// controller is tearing down the temp PVC (and optionally the
	// VolumeSnapshot).
	PVCBackupPhaseCleanup PVCBackupPhase = "Cleanup"

	// PVCBackupPhaseSynced means the last run finished successfully and
	// the controller is back to Idle until the next fire time. This phase
	// is set briefly between runs for status-subresource consumers; the
	// controller transitions back to Idle on the next reconcile.
	PVCBackupPhaseSynced PVCBackupPhase = "Synced"

	// PVCBackupPhaseFailed means the last run failed. The controller will
	// still honor the schedule and attempt again at the next fire time.
	PVCBackupPhaseFailed PVCBackupPhase = "Failed"
)

// PVCBackupSpec defines the backup policy for a single PVC. The object is
// typically owned by a BackupConfig reconciler that creates/updates one
// PVCBackup per matching PVC, but users may also author PVCBackup objects
// directly for one-off or bespoke schedules.
type PVCBackupSpec struct {
	// PVCName is the name of the source PersistentVolumeClaim in the same
	// namespace as this PVCBackup.
	PVCName string `json:"pvcName"`

	// Schedule is a cron expression evaluated in-process by the controller.
	// Empty disables scheduling; the controller will not take backups.
	//+kubebuilder:validation:Optional
	Schedule string `json:"schedule,omitempty"`

	// Retention is the restic forget policy applied after each successful
	// backup.
	//+kubebuilder:validation:Optional
	Retention RetentionSpec `json:"retention,omitempty"`

	// KeepSnapshotAfterBackup leaves the VolumeSnapshot in place after the
	// mover completes. The temp PVC is always deleted.
	//+kubebuilder:validation:Optional
	KeepSnapshotAfterBackup bool `json:"keepSnapshotAfterBackup,omitempty"`

	// VolumeSnapshotClassName is the VolumeSnapshotClass used for the
	// backup snapshot. If empty the default class for the PVC's driver is
	// used.
	//+kubebuilder:validation:Optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`

	// Suspend pauses new backup runs without affecting the current run or
	// existing restic data.
	//+kubebuilder:validation:Optional
	Suspend bool `json:"suspend,omitempty"`

	// Trigger forces a one-shot run. Setting this to a new value that
	// differs from status.lastHandledTrigger causes the controller to run a
	// backup immediately, regardless of the schedule. A common pattern is
	// to set it to a timestamp. Empty means no manual trigger.
	//+kubebuilder:validation:Optional
	Trigger string `json:"trigger,omitempty"`
}

// MoverStatus captures the outcome of the last mover Job.
type MoverStatus struct {
	//+kubebuilder:validation:Optional
	JobName string `json:"jobName,omitempty"`

	//+kubebuilder:validation:Optional
	Succeeded bool `json:"succeeded,omitempty"`

	//+kubebuilder:validation:Optional
	Logs string `json:"logs,omitempty"`
}

// PVCBackupStatus reports the observed state of a PVCBackup.
type PVCBackupStatus struct {
	//+kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	//+kubebuilder:validation:Optional
	Phase PVCBackupPhase `json:"phase,omitempty"`

	//+kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`

	// LastSyncTime is when the most recent backup completed successfully.
	//+kubebuilder:validation:Optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// LastSyncDuration is how long the most recent successful backup took.
	//+kubebuilder:validation:Optional
	LastSyncDuration *metav1.Duration `json:"lastSyncDuration,omitempty"`

	// NextSyncTime is the next scheduled fire time based on .spec.schedule.
	//+kubebuilder:validation:Optional
	NextSyncTime *metav1.Time `json:"nextSyncTime,omitempty"`

	// LastHandledTrigger mirrors .spec.trigger the last time a manual run
	// was accepted, so repeated reconciles of the same trigger value don't
	// retrigger backups.
	//+kubebuilder:validation:Optional
	LastHandledTrigger string `json:"lastHandledTrigger,omitempty"`

	// CurrentRunStartTime is the .metadata.creationTimestamp of the in-flight
	// VolumeSnapshot/temp PVC/mover Job, if any. Nil when phase=Idle|Synced|Failed.
	//+kubebuilder:validation:Optional
	CurrentRunStartTime *metav1.Time `json:"currentRunStartTime,omitempty"`

	// CurrentSnapshotName, CurrentTempPVCName, CurrentMoverJobName track
	// the child resources owned by this PVCBackup during an active run.
	//+kubebuilder:validation:Optional
	CurrentSnapshotName string `json:"currentSnapshotName,omitempty"`

	//+kubebuilder:validation:Optional
	CurrentTempPVCName string `json:"currentTempPVCName,omitempty"`

	//+kubebuilder:validation:Optional
	CurrentMoverJobName string `json:"currentMoverJobName,omitempty"`

	// LatestMoverStatus mirrors the final state of the most recently completed mover Job.
	//+kubebuilder:validation:Optional
	LatestMoverStatus *MoverStatus `json:"latestMoverStatus,omitempty"`

	//+kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,shortName=pvcbk
//+kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.spec.pvcName`
//+kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="LastSync",type=date,JSONPath=`.status.lastSyncTime`
//+kubebuilder:printcolumn:name="NextSync",type=date,JSONPath=`.status.nextSyncTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PVCBackup represents an ongoing backup policy for a single PVC. The
// controller evaluates .spec.schedule in-process, takes a VolumeSnapshot at
// each fire time, provisions a temp PVC from the snapshot, runs a restic
// mover Job against it, and cleans up the temp PVC (and optionally the
// VolumeSnapshot). All child resources are owned by the PVCBackup so
// deleting it cascades cleanly.
type PVCBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PVCBackupSpec   `json:"spec,omitempty"`
	Status PVCBackupStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PVCBackupList contains a list of PVCBackup.
type PVCBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PVCBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PVCBackup{}, &PVCBackupList{})
}
