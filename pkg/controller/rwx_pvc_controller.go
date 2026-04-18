package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SetupRWXPersistentVolumeClaimReconciler(
	mgr ctrl.Manager,
	c client.Client,
	apiReader client.Reader,
	ganeshaImage string,
) error {
	reconciler := internalController.NewRWXPersistentVolumeClaimReconciler(c, apiReader, ganeshaImage)
	return reconciler.SetupWithManager(mgr)
}
