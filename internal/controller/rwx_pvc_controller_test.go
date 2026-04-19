package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/internal/nfs"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var _ = Describe("RWXPersistentVolumeClaimController controller", func() {
	ctx := context.Background()
	var stopFunc func()
	errCh := make(chan error)

	BeforeEach(func() {
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme,
			Controller: config.Controller{
				SkipNameValidation: &skipNameValidation,
			},
			Metrics: server.Options{BindAddress: "0"},
		})
		Expect(err).ToNot(HaveOccurred())

		r := NewRWXPersistentVolumeClaimReconciler(k8sClient, mgr.GetAPIReader(), "")
		Expect(r.SetupWithManager(mgr)).NotTo(HaveOccurred())

		ctx, cancel := context.WithCancel(ctx)
		stopFunc = cancel
		go func() {
			errCh <- mgr.Start(ctx)
		}()
		time.Sleep(100 * time.Millisecond)
	})

	AfterEach(func() {
		stopFunc()
		Expect(<-errCh).NotTo(HaveOccurred())
	})

	It("ignores PVCs whose StorageClass is not an RWX TopoLVM class", func() {
		ns := createNamespace()

		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "unrelated"},
			Provisioner: "other.io",
			Parameters: map[string]string{
				topolvm.RWXAccessModeParameter: topolvm.RWXAccessModeValue,
			},
		}
		Expect(k8sClient.Create(ctx, sc)).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated-pvc", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: ptr.To("unrelated"),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).NotTo(HaveOccurred())

		Consistently(func() error {
			backing := &corev1.PersistentVolumeClaim{}
			return k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.BackingPVCName(pvc.Name),
			}, backing)
		}, 2*time.Second, 200*time.Millisecond).Should(WithTransform(apierrors.IsNotFound, BeTrue()))
	})

	It("creates the NFS stack and finalizer for a valid RWX PVC", func() {
		ns := createNamespace()

		rwxSC := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: storageClassNameBase + "-rwx"},
			Provisioner: topolvm.GetPluginName(),
			Parameters: map[string]string{
				topolvm.RWXAccessModeParameter:          topolvm.RWXAccessModeValue,
				topolvm.RWXBackingStorageClassParameter: storageClassNameBase + "-rwo",
			},
		}
		Expect(k8sClient.Create(ctx, rwxSC)).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "rwx-pvc", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: ptr.To(rwxSC.Name),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			got := &corev1.PersistentVolumeClaim{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), got)).To(Succeed())
			g.Expect(got.Finalizers).To(ContainElement(topolvm.RWXFinalizer))
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			backing := &corev1.PersistentVolumeClaim{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.BackingPVCName(pvc.Name),
			}, backing)).To(Succeed())
			g.Expect(backing.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.ServerName(pvc.Name),
			}, dep)).To(Succeed())
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			svc := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.ServerName(pvc.Name),
			}, svc)).To(Succeed())
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			pv := &corev1.PersistentVolume{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: nfs.PVName(ns, pvc.Name),
			}, pv)).To(Succeed())
			g.Expect(pv.Spec.CSI).NotTo(BeNil())
			g.Expect(pv.Spec.CSI.Driver).To(Equal(topolvm.NFSCSIDriverName))
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.ConfigMapName(pvc.Name),
			}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKey("ganesha.conf"))
		}).Should(Succeed())
	})

	It("rejects an RWX StorageClass that lacks a backing class reference", func() {
		ns := createNamespace()

		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: storageClassNameBase + "-rwx-missing"},
			Provisioner: topolvm.GetPluginName(),
			Parameters: map[string]string{
				topolvm.RWXAccessModeParameter: topolvm.RWXAccessModeValue,
			},
		}
		Expect(k8sClient.Create(ctx, sc)).NotTo(HaveOccurred())

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "missing-backing", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: ptr.To(sc.Name),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).NotTo(HaveOccurred())

		Consistently(func() error {
			backing := &corev1.PersistentVolumeClaim{}
			return k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      nfs.BackingPVCName(pvc.Name),
			}, backing)
		}, 2*time.Second, 200*time.Millisecond).Should(WithTransform(apierrors.IsNotFound, BeTrue()))
	})
})
