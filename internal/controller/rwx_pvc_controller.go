package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/internal/nfs"
	appsv1 "k8s.io/api/apps/v1"
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

type RWXPersistentVolumeClaimReconciler struct {
	client       client.Client
	apiReader    client.Reader
	ganeshaImage string
}

func NewRWXPersistentVolumeClaimReconciler(c client.Client, apiReader client.Reader, ganeshaImage string) *RWXPersistentVolumeClaimReconciler {
	return &RWXPersistentVolumeClaimReconciler{
		client:       c,
		apiReader:    apiReader,
		ganeshaImage: ganeshaImage,
	}
}

//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

func (r *RWXPersistentVolumeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, req.NamespacedName, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	isRWX, sc, err := r.isRWXPVC(ctx, pvc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isRWX {
		return ctrl.Result{}, nil
	}

	if pvc.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, pvc)
	}

	if !controllerutil.ContainsFinalizer(pvc, topolvm.RWXFinalizer) {
		controllerutil.AddFinalizer(pvc, topolvm.RWXFinalizer)
		if err := r.client.Update(ctx, pvc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	cfg, err := r.buildConfig(pvc, sc)
	if err != nil {
		log.Error(err, "unable to build RWX config", "pvc", req.NamespacedName)
		return ctrl.Result{}, err
	}

	if err := r.ensureConfigMap(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure ganesha configmap: %w", err)
	}
	if err := r.ensureBackingPVC(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure backing PVC: %w", err)
	}
	if err := r.ensureDeployment(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure ganesha deployment: %w", err)
	}
	svc, err := r.ensureService(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure nfs service: %w", err)
	}
	if err := r.ensurePV(ctx, cfg, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure nfs PV: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *RWXPersistentVolumeClaimReconciler) isRWXPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, *storagev1.StorageClass, error) {
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

func (r *RWXPersistentVolumeClaimReconciler) buildConfig(pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) (nfs.Config, error) {
	size, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return nfs.Config{}, fmt.Errorf("PVC %s/%s has no storage request", pvc.Namespace, pvc.Name)
	}

	backingSC := sc.Parameters[topolvm.RWXBackingStorageClassParameter]
	if backingSC == "" {
		return nfs.Config{}, fmt.Errorf(
			"StorageClass %q is RWX but does not set %q",
			sc.Name, topolvm.RWXBackingStorageClassParameter,
		)
	}

	return nfs.Config{
		UserPVCNamespace:    pvc.Namespace,
		UserPVCName:         pvc.Name,
		UserPVCUID:          string(pvc.UID),
		Size:                size,
		BackingStorageClass: backingSC,
		GaneshaImage:        r.ganeshaImage,
		DataSource:          translateDataSource(pvc.Spec.DataSource),
	}, nil
}

func translateDataSource(src *corev1.TypedLocalObjectReference) *corev1.TypedLocalObjectReference {
	if src == nil {
		return nil
	}
	group := ""
	if src.APIGroup != nil {
		group = *src.APIGroup
	}
	switch {
	case src.Kind == "VolumeSnapshot" && group == "snapshot.storage.k8s.io":
		return &corev1.TypedLocalObjectReference{
			APIGroup: src.APIGroup,
			Kind:     src.Kind,
			Name:     src.Name + topolvm.RWXMirrorSnapshotSuffix,
		}
	case src.Kind == "PersistentVolumeClaim" && group == "":
		return &corev1.TypedLocalObjectReference{
			APIGroup: src.APIGroup,
			Kind:     src.Kind,
			Name:     nfs.BackingPVCName(src.Name),
		}
	default:
		return src
	}
}

func (r *RWXPersistentVolumeClaimReconciler) ensureConfigMap(ctx context.Context, cfg nfs.Config) error {
	want := nfs.ConfigMap(cfg)
	got := &corev1.ConfigMap{}
	err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.client.Create(ctx, want)
	}
	return err
}

func (r *RWXPersistentVolumeClaimReconciler) ensureBackingPVC(ctx context.Context, cfg nfs.Config) error {
	want := nfs.BackingPVC(cfg)
	got := &corev1.PersistentVolumeClaim{}
	err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.client.Create(ctx, want)
	}
	if err != nil {
		return err
	}

	current, ok := got.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return nil
	}
	if cfg.Size.Cmp(current) > 0 {
		got.Spec.Resources.Requests[corev1.ResourceStorage] = cfg.Size
		return r.client.Update(ctx, got)
	}
	return nil
}

func (r *RWXPersistentVolumeClaimReconciler) ensureDeployment(ctx context.Context, cfg nfs.Config) error {
	want := nfs.Deployment(cfg)
	got := &appsv1.Deployment{}
	err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.client.Create(ctx, want)
	}
	return err
}

func (r *RWXPersistentVolumeClaimReconciler) ensureService(ctx context.Context, cfg nfs.Config) (*corev1.Service, error) {
	want := nfs.Service(cfg)
	got := &corev1.Service{}
	err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.client.Create(ctx, want); err != nil {
			return nil, err
		}
		if err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got); err != nil {
			return nil, err
		}
		return got, nil
	case err != nil:
		return nil, err
	default:
		return got, nil
	}
}

func (r *RWXPersistentVolumeClaimReconciler) ensurePV(ctx context.Context, cfg nfs.Config, svc *corev1.Service) error {
	want := nfs.PV(cfg, svc.Spec.ClusterIP)
	got := &corev1.PersistentVolume{}
	err := r.client.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.client.Create(ctx, want)
	}
	if err != nil {
		return err
	}

	current := got.Spec.Capacity[corev1.ResourceStorage]
	if cfg.Size.Cmp(current) > 0 {
		got.Spec.Capacity[corev1.ResourceStorage] = cfg.Size
		return r.client.Update(ctx, got)
	}
	return nil
}

func (r *RWXPersistentVolumeClaimReconciler) reconcileDelete(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pvc, topolvm.RWXFinalizer) {
		return ctrl.Result{}, nil
	}

	pvName := nfs.PVName(pvc.Namespace, pvc.Name)
	svcName := nfs.ServerName(pvc.Name)
	backingName := nfs.BackingPVCName(pvc.Name)
	cmName := nfs.ConfigMapName(pvc.Name)

	if err := r.deleteIfExists(ctx, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvName}}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete RWX PV: %w", err)
	}
	if err := r.deleteIfExists(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: pvc.Namespace}}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete NFS service: %w", err)
	}
	if err := r.deleteIfExists(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: pvc.Namespace}}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete ganesha deployment: %w", err)
	}
	if err := r.deleteIfExists(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: pvc.Namespace}}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete ganesha configmap: %w", err)
	}
	if err := r.deleteIfExists(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: backingName, Namespace: pvc.Namespace}}); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete backing PVC: %w", err)
	}

	controllerutil.RemoveFinalizer(pvc, topolvm.RWXFinalizer)
	if err := r.client.Update(ctx, pvc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RWXPersistentVolumeClaimReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	err := r.client.Delete(ctx, obj)
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *RWXPersistentVolumeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("rwx-persistentvolumeclaim").
		WithEventFilter(pred).
		For(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
