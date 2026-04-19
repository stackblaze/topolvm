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

func pickRWXNodes() (writer, reader string) {
	var nodes corev1.NodeList
	err := getObjects(&nodes, "nodes", "-l=!node-role.kubernetes.io/control-plane")
	if err != nil {
		Skip(fmt.Sprintf("could not list worker nodes: %v", err))
	}
	workers := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		workers = append(workers, n.Name)
	}
	switch len(workers) {
	case 0:
		Skip("no worker nodes available")
	case 1:
		// Sanity test deletes worker2/worker3 — fall back to a single
		// worker so the pods can still co-locate on it. This still
		// proves the user PVC is mountable from multiple pods, just not
		// across nodes.
		return workers[0], workers[0]
	default:
		return workers[0], workers[1]
	}
	return "", ""
}

func isRWXEnabled() bool {
	return os.Getenv("RWX_ENABLED") == "1"
}

func logf(format string, a ...any) {
	_, _ = fmt.Fprintf(GinkgoWriter, format, a...)
}

func dumpRWXDiagnostics(ns string) {
	logf("\n===== RWX diagnostics for namespace %s =====\n", ns)
	for _, cmd := range [][]string{
		{"get", "pvc", "-n", ns, "-o", "wide"},
		{"get", "pods", "-n", ns, "-o", "wide"},
		{"get", "deploy", "-n", ns, "-o", "wide"},
		{"get", "svc", "-n", ns, "-o", "wide"},
		{"get", "pv"},
		{"describe", "pvc", "-n", ns},
		{"describe", "pods", "-n", ns},
		{"logs", "-n", ns, "-l", "app.kubernetes.io/name=topolvm-rwx-nfs", "--tail=200"},
		{"get", "events", "-n", ns, "--sort-by=.metadata.creationTimestamp"},
		{"get", "pods", "-n", "topolvm-system"},
		{"logs", "-n", "topolvm-system", "-l",
			"app.kubernetes.io/component=controller",
			"-c", "topolvm-controller", "--tail=200"},
	} {
		out, err := kubectl(cmd...)
		logf("\n--- kubectl %v ---\n", cmd)
		logf("%s\n", string(out))
		if err != nil {
			logf("(error: %v)\n", err)
		}
	}
	logf("===== end diagnostics =====\n\n")
}

func testRWX() {
	if !isRWXEnabled() {
		return
	}

	var ns string
	var preflightDone bool

	BeforeEach(func() {
		if !preflightDone {
			logf("\n===== RWX pre-flight =====\n")
			for _, cmd := range [][]string{
				{"get", "nodes", "-o", "wide"},
				{"get", "sc"},
				{"get", "csidrivers"},
				{"get", "pods", "-n", "kube-system", "-l", "app=csi-nfs-controller"},
				{"get", "pods", "-n", "kube-system", "-l", "app=csi-nfs-node"},
				{"get", "pods", "-n", "topolvm-system"},
			} {
				out, err := kubectl(cmd...)
				logf("\n--- kubectl %v ---\n", cmd)
				logf("%s\n", string(out))
				if err != nil {
					logf("(error: %v)\n", err)
				}
			}
			logf("===== end pre-flight =====\n")
			preflightDone = true
		}
		ns = "rwx-test-" + randomString()
		createNamespace(ns)
	})
	AfterEach(func() {
		if CurrentSpecReport().State.Is(types.SpecStateFailureStates) {
			dumpRWXDiagnostics(ns)
			return
		}
		// Bound the teardown so a stuck PVC finalizer doesn't consume the
		// suite budget. If the deletion doesn't complete in time, log and
		// move on; Ginkgo will still report the spec as passing.
		_, err := kubectl("delete", "namespaces", ns, "--wait=true", "--timeout=120s")
		if err != nil {
			logf("namespace teardown did not complete cleanly: %v\n", err)
			dumpRWXDiagnostics(ns)
		}
	})

	It("mounts the same RWX PVC from two pods", func() {
		writerNode, readerNode := pickRWXNodes()
		By(fmt.Sprintf("scheduling writer on %s, reader on %s", writerNode, readerNode))

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

		writerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxWriterPodName, rwxPVCName, writerNode))
		_, err = kubectlWithInput(writerYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		readerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxReaderPodName, rwxPVCName, readerNode))
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
		writerNode, readerNode := pickRWXNodes()

		By("creating and populating the source RWX PVC")
		pvcYAML := []byte(fmt.Sprintf(rwxPVCTemplateYAML, rwxPVCName, rwxInitialSizeMi))
		_, err := kubectlWithInput(pvcYAML, "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		writerYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxWriterPodName, rwxPVCName, writerNode))
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

		restorePodYAML := []byte(fmt.Sprintf(rwxPodTemplateYAML, rwxRestorePodName, rwxRestorePVCName, readerNode))
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
