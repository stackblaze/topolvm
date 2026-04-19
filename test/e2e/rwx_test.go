package e2e

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	snapapi "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

var (
	//go:embed testdata/rwx/pvc-template.yaml
	rwxPVCTemplateYAML string

	//go:embed testdata/rwx/pod-template.yaml
	rwxPodTemplateYAML string

	//go:embed testdata/rwx/snapshot-template.yaml
	rwxSnapshotTemplateYAML string

	//go:embed testdata/rwx/restore-pvc-template.yaml
	rwxRestorePVCTemplateYAML string
)

const (
	rwxPVCName           = "rwx-shared"
	rwxWriterPodName     = "rwx-writer"
	rwxReaderPodName     = "rwx-reader"
	rwxWriterNode        = "topolvm-e2e-worker2"
	rwxReaderNode        = "topolvm-e2e-worker3"
	rwxSnapName          = "rwx-snap"
	rwxSnapClass         = "topolvm-provisioner-thin"
	rwxRestorePVCName    = "rwx-restored"
	rwxRestorePodName    = "rwx-restore-reader"
	rwxInitialSizeMi     = 256
	rwxExpandedSizeMi    = 512
	rwxRestoreSizeMi     = 256
	rwxSharedFile        = "/shared/hello.txt"
	rwxSharedFileContent = "hello-from-rwx"
)

func isRWXEnabled() bool {
	return os.Getenv("RWX_ENABLED") == "1"
}

func testRWX() {
	if !isRWXEnabled() {
		return
	}

	var ns string

	BeforeEach(func() {
		ns = "rwx-test-" + randomString()
		createNamespace(ns)
	})
	AfterEach(func() {
		if !CurrentSpecReport().State.Is(types.SpecStateFailureStates) {
			_, err := kubectl("delete", "namespaces", ns)
			Expect(err).ShouldNot(HaveOccurred())
		}
	})

	It("mounts the same RWX PVC from two pods on different nodes", func() {
		By("creating the RWX PVC")
		pvcYAML := []byte(fmt.Sprintf(rwxPVCTemplateYAML, rwxPVCName, rwxInitialSizeMi))
		_, err := kubectlWithInput(pvcYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("waiting for the PVC to bind")
		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			if err := getObjects(&pvc, "pvc", "-n", ns, rwxPVCName); err != nil {
				return err
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC not bound: %s", pvc.Status.Phase)
			}
			return nil
		}).Should(Succeed())

		By("scheduling writer and reader pods on different nodes")
		writerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxWriterPodName, rwxPVCName, rwxWriterNode))
		_, err = kubectlWithInput(writerYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		readerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxReaderPodName, rwxPVCName, rwxReaderNode))
		_, err = kubectlWithInput(readerYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			for _, pod := range []string{rwxWriterPodName, rwxReaderPodName} {
				var p corev1.Pod
				if err := getObjects(&p, "pod", "-n", ns, pod); err != nil {
					return err
				}
				if p.Status.Phase != corev1.PodRunning {
					return fmt.Errorf("%s not running: %s", pod, p.Status.Phase)
				}
			}
			return nil
		}).Should(Succeed())

		By("writing from the writer pod")
		_, err = kubectl("exec", "-n", ns, rwxWriterPodName, "--", "sh", "-c",
			fmt.Sprintf("echo -n %q > %s && sync", rwxSharedFileContent, rwxSharedFile))
		Expect(err).ShouldNot(HaveOccurred())

		By("reading the same file from the reader pod on a different node")
		Eventually(func() error {
			out, err := kubectl("exec", "-n", ns, rwxReaderPodName, "--", "cat", rwxSharedFile)
			if err != nil {
				return err
			}
			got := strings.TrimSpace(string(out))
			if got != rwxSharedFileContent {
				return fmt.Errorf("unexpected content %q", got)
			}
			return nil
		}).Should(Succeed())
	})

	It("snapshots and restores an RWX PVC", func() {
		By("creating and populating the source RWX PVC")
		pvcYAML := []byte(fmt.Sprintf(rwxPVCTemplateYAML, rwxPVCName, rwxInitialSizeMi))
		_, err := kubectlWithInput(pvcYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		writerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxWriterPodName, rwxPVCName, rwxWriterNode))
		_, err = kubectlWithInput(writerYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			var p corev1.Pod
			if err := getObjects(&p, "pod", "-n", ns, rwxWriterPodName); err != nil {
				return err
			}
			if p.Status.Phase != corev1.PodRunning {
				return fmt.Errorf("%s not running: %s", rwxWriterPodName, p.Status.Phase)
			}
			return nil
		}).Should(Succeed())

		_, err = kubectl("exec", "-n", ns, rwxWriterPodName, "--", "sh", "-c",
			fmt.Sprintf("echo -n %q > %s && sync", rwxSharedFileContent, rwxSharedFile))
		Expect(err).ShouldNot(HaveOccurred())

		By("creating a snapshot of the RWX PVC")
		snapYAML := []byte(fmt.Sprintf(rwxSnapshotTemplateYAML, rwxSnapName, rwxSnapClass, rwxPVCName))
		_, err = kubectlWithInput(snapYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("waiting for the mirror snapshot to be ready")
		var mirror snapapi.VolumeSnapshot
		Eventually(func() error {
			err := getObjects(&mirror, "vs", "-n", ns, rwxSnapName+"-rwx-backing")
			if err != nil {
				return err
			}
			if mirror.Status == nil || mirror.Status.ReadyToUse == nil || !*mirror.Status.ReadyToUse {
				return fmt.Errorf("mirror snapshot not ready")
			}
			return nil
		}).Should(Succeed())

		By("restoring into a new RWX PVC")
		restoreYAML := []byte(fmt.Sprintf(rwxRestorePVCTemplateYAML, rwxRestorePVCName, rwxRestoreSizeMi, rwxSnapName))
		_, err = kubectlWithInput(restoreYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		restorePodYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxRestorePodName, rwxRestorePVCName, rwxReaderNode))
		_, err = kubectlWithInput(restorePodYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("reading the snapshotted file through the restored PVC")
		Eventually(func() error {
			out, err := kubectl("exec", "-n", ns, rwxRestorePodName, "--", "cat", rwxSharedFile)
			if err != nil {
				return err
			}
			got := strings.TrimSpace(string(out))
			if got != rwxSharedFileContent {
				return fmt.Errorf("unexpected content %q", got)
			}
			return nil
		}).Should(Succeed())
	})

	It("expands an RWX PVC online", func() {
		By("creating the RWX PVC")
		pvcYAML := []byte(fmt.Sprintf(rwxPVCTemplateYAML, rwxPVCName, rwxInitialSizeMi))
		_, err := kubectlWithInput(pvcYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			if err := getObjects(&pvc, "pvc", "-n", ns, rwxPVCName); err != nil {
				return err
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC not bound: %s", pvc.Status.Phase)
			}
			return nil
		}).Should(Succeed())

		By("patching the PVC size")
		patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%dMi"}}}}`, rwxExpandedSizeMi)
		_, err = kubectl("patch", "pvc", "-n", ns, rwxPVCName, "--type", "merge", "-p", patch)
		Expect(err).ShouldNot(HaveOccurred())

		By("waiting for the backing PVC to reach the expanded size")
		Eventually(func() error {
			var backing corev1.PersistentVolumeClaim
			if err := getObjects(&backing, "pvc", "-n", ns, rwxPVCName+"-rwx-backing"); err != nil {
				return err
			}
			req := backing.Spec.Resources.Requests[corev1.ResourceStorage]
			want := fmt.Sprintf("%dMi", rwxExpandedSizeMi)
			if req.String() != want {
				return fmt.Errorf("backing request %s != %s", req.String(), want)
			}
			return nil
		}).Should(Succeed())
	})
}
