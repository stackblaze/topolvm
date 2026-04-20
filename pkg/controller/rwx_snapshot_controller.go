package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SetupRWXVolumeSnapshotReconciler(mgr ctrl.Manager, c client.Client, apiReader client.Reader) error {
	reconciler := internalController.NewRWXVolumeSnapshotReconciler(c, apiReader)
	return reconciler.SetupWithManager(mgr)
}
