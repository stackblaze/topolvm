package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RestorePhase captures the coarse state of a Restore.
type RestorePhase string

const (
	RestorePhasePending    RestorePhase = "Pending"
	RestorePhaseRunning    RestorePhase = "Running"
	RestorePhaseSucceeded  RestorePhase = "Succeeded"
	RestorePhaseFailed     RestorePhase = "Failed"
)

// RestoreSpec defines a single restore operation from a restic repository
// back into a fresh PersistentVolumeClaim.
type RestoreSpec struct {
	// SourceNamespace is the namespace of the PVC whose backups should be
	// restored. The restic repository key is "<sourceNamespace>/<sourcePVCName>".
	SourceNamespace string `json:"sourceNamespace"`

	// SourcePVCName is the name of the original PVC that was backed up.
	SourcePVCName string `json:"sourcePVCName"`

	// SnapshotID is the restic snapshot id to restore. If empty, "latest" is
	// used.
	//+kubebuilder:validation:Optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// TargetPVCName is the name of the PVC to create in this Restore's
	// namespace. The controller provisions this PVC with the given size and
	// storage class, then mounts it into the restic restore Job.
	TargetPVCName string `json:"targetPVCName"`

	// Size is the capacity for the target PVC. It should be at least as large
	// as the source.
	Size resource.Quantity `json:"size"`

	// StorageClassName is the StorageClass for the target PVC. Must reference
	// a TopoLVM StorageClass.
	StorageClassName string `json:"storageClassName"`
}

// RestoreStatus reports the observed state of a Restore.
type RestoreStatus struct {
	//+kubebuilder:validation:Optional
	Phase RestorePhase `json:"phase,omitempty"`

	// Message is a human-readable explanation of the current phase.
	//+kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`

	// JobName is the name of the Job running the restic restore.
	//+kubebuilder:validation:Optional
	JobName string `json:"jobName,omitempty"`

	// StartTime is when the restore Job was created.
	//+kubebuilder:validation:Optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the restore Job reached a terminal state.
	//+kubebuilder:validation:Optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions is the list of conditions for this Restore.
	//+kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourcePVCName`
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetPVCName`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Restore is the Schema for restic-backed PVC restore operations.
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec,omitempty"`
	Status RestoreStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// RestoreList contains a list of Restore.
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Restore{}, &RestoreList{})
}
