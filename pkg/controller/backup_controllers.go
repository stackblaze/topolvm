package controller

import (
	internalController "github.com/topolvm/topolvm/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetupBackupReconcilers wires the three backup/restore reconcilers:
// BackupConfig (PVC -> PVCBackup + Secret propagation), PVCBackup (runs the
// actual backup state machine), and Restore (one-shot restore flow).
//
// controllerNamespace is the namespace where the user-authored S3 creds and
// restic password Secrets live; the BackupConfig controller mirrors them
// into every namespace that has a managed PVC.
func SetupBackupReconcilers(
	mgr ctrl.Manager,
	c client.Client,
	apiReader client.Reader,
	controllerNamespace, resticImage, moverServiceAccountName string,
) error {
	bc := internalController.NewBackupConfigReconciler(
		c, apiReader, controllerNamespace, resticImage, moverServiceAccountName,
	)
	if err := bc.SetupWithManager(mgr); err != nil {
		return err
	}

	pb := internalController.NewPVCBackupReconciler(c, apiReader, bc.ResolveRuntimeConfig)
	if err := pb.SetupWithManager(mgr); err != nil {
		return err
	}

	rs := internalController.NewRestoreReconciler(
		c, apiReader, bc.LoadSingleton,
		controllerNamespace, resticImage, moverServiceAccountName,
	)
	return rs.SetupWithManager(mgr)
}
