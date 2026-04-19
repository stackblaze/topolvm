package controller

import (
	"context"
	"fmt"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/internal/nfs"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type RWXVolumeSnapshotReconciler struct {
	client    client.Client
	apiReader client.Reader
}

func NewRWXVolumeSnapshotReconciler(c client.Client, apiReader client.Reader) *RWXVolumeSnapshotReconciler {
	return &RWXVolumeSnapshotReconciler{client: c, apiReader: apiReader}
}

//+kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete

func (r *RWXVolumeSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	userSnap := &snapshotv1.VolumeSnapshot{}
	if err := r.client.Get(ctx, req.NamespacedName, userSnap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	sourcePVCName := userSnap.Spec.Source.PersistentVolumeClaimName
	if sourcePVCName == nil || *sourcePVCName == "" {
		return ctrl.Result{}, nil
	}

	sourcePVC := &corev1.PersistentVolumeClaim{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: userSnap.Namespace, Name: *sourcePVCName}, sourcePVC)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	isRWX, _, err := r.isRWXPVC(ctx, sourcePVC)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isRWX {
		return ctrl.Result{}, nil
	}

	if userSnap.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, userSnap)
	}

	if !controllerutil.ContainsFinalizer(userSnap, topolvm.RWXSnapshotFinalizer) {
		controllerutil.AddFinalizer(userSnap, topolvm.RWXSnapshotFinalizer)
		if err := r.client.Update(ctx, userSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.ensureMirrorSnapshot(ctx, userSnap, sourcePVC); err != nil {
		log.Error(err, "failed to ensure mirror snapshot",
			"userSnapshot", req.NamespacedName)
		return ctrl.Result{}, fmt.Errorf("ensure mirror snapshot: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *RWXVolumeSnapshotReconciler) isRWXPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, *storagev1.StorageClass, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false, nil, nil
	}
	sc := &storagev1.StorageClass{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	if sc.Provisioner != topolvm.RWXProvisionerName {
		return false, nil, nil
	}
	if !strings.EqualFold(sc.Parameters[topolvm.RWXAccessModeParameter], topolvm.RWXAccessModeValue) {
		return false, nil, nil
	}
	return true, sc, nil
}

func (r *RWXVolumeSnapshotReconciler) ensureMirrorSnapshot(ctx context.Context, userSnap *snapshotv1.VolumeSnapshot, sourcePVC *corev1.PersistentVolumeClaim) error {
	mirrorName := userSnap.Name + topolvm.RWXMirrorSnapshotSuffix
	backingPVC := nfs.BackingPVCName(sourcePVC.Name)

	mirror := &snapshotv1.VolumeSnapshot{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: userSnap.Namespace, Name: mirrorName}, mirror)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	want := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mirrorName,
			Namespace: userSnap.Namespace,
			Labels: map[string]string{
				topolvm.CreatedbyLabelKey:              topolvm.CreatedbyLabelValue,
				topolvm.RWXManagedByLabel:              "topolvm-rwx",
				topolvm.RWXOwnerSnapshotNamespaceLabel: userSnap.Namespace,
				topolvm.RWXOwnerSnapshotNameLabel:      userSnap.Name,
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: userSnap.Spec.VolumeSnapshotClassName,
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &backingPVC,
			},
		},
	}
	return r.client.Create(ctx, want)
}

func (r *RWXVolumeSnapshotReconciler) reconcileDelete(ctx context.Context, userSnap *snapshotv1.VolumeSnapshot) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(userSnap, topolvm.RWXSnapshotFinalizer) {
		return ctrl.Result{}, nil
	}
	mirrorName := userSnap.Name + topolvm.RWXMirrorSnapshotSuffix
	mirror := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: mirrorName, Namespace: userSnap.Namespace},
	}
	err := r.client.Delete(ctx, mirror)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete mirror snapshot: %w", err)
	}

	controllerutil.RemoveFinalizer(userSnap, topolvm.RWXSnapshotFinalizer)
	if err := r.client.Update(ctx, userSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RWXVolumeSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("rwx-volumesnapshot").
		WithEventFilter(pred).
		For(&snapshotv1.VolumeSnapshot{}).
		Complete(r)
}
