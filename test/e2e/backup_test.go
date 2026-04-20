package e2e

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

var (
	//go:embed testdata/backup/pvc-template.yaml
	backupPVCTemplateYAML string

	//go:embed testdata/backup/writer-pod-template.yaml
	backupWriterPodTemplateYAML string

	//go:embed testdata/backup/reader-pod-template.yaml
	backupReaderPodTemplateYAML string

	//go:embed testdata/backup/s3-creds-secret.yaml
	backupS3CredsSecretYAML string

	//go:embed testdata/backup/restic-password-secret.yaml
	backupResticPasswordSecretYAML string

	//go:embed testdata/backup/backupconfig-template.yaml
	backupConfigTemplateYAML string

	//go:embed testdata/backup/restore-template.yaml
	backupRestoreTemplateYAML string
)

const (
	backupPVCName         = "backup-src"
	backupWriterPodName   = "backup-writer"
	backupReaderPodName   = "backup-reader"
	backupRestoreName     = "backup-restore"
	backupRestorePVCName  = "backup-restored"
	backupS3CredsSecret   = "s3-creds"
	backupResticPwSecret  = "restic-pw"
	backupPVCSizeMi       = 512
	backupFileContents    = "hello-from-backup"
	backupControllerNS    = "topolvm-system"
)

type backupEnv struct {
	endpoint       string
	bucket         string
	accessKey      string
	secretKey      string
	resticPassword string
	prefix         string
}

func (e backupEnv) complete() bool {
	return e.endpoint != "" && e.bucket != "" &&
		e.accessKey != "" && e.secretKey != "" && e.resticPassword != ""
}

func loadBackupEnv() backupEnv {
	return backupEnv{
		endpoint:       os.Getenv("BACKUP_S3_ENDPOINT"),
		bucket:         os.Getenv("BACKUP_S3_BUCKET"),
		accessKey:      os.Getenv("BACKUP_S3_ACCESS_KEY"),
		secretKey:      os.Getenv("BACKUP_S3_SECRET_KEY"),
		resticPassword: os.Getenv("BACKUP_RESTIC_PASSWORD"),
		prefix:         os.Getenv("BACKUP_S3_PREFIX"),
	}
}

func isBackupEnabled() bool {
	return os.Getenv("BACKUP_ENABLED") == "1"
}

func dumpBackupDiagnostics(ns string) {
	logf("\n===== backup diagnostics for namespace %s =====\n", ns)
	for _, cmd := range [][]string{
		{"get", "pvc", "-n", ns, "-o", "wide"},
		{"get", "pods", "-n", ns, "-o", "wide"},
		{"get", "volumesnapshot", "-n", ns},
		{"get", "jobs", "-n", ns},
		{"get", "pvcbackup", "-n", ns, "-o", "yaml"},
		{"get", "restore", "-n", ns, "-o", "yaml"},
		{"describe", "pvcbackup", "-n", ns},
		{"describe", "restore", "-n", ns},
		{"get", "events", "-n", ns, "--sort-by=.metadata.creationTimestamp"},
		{"get", "backupconfig"},
		{"get", "pods", "-n", backupControllerNS},
		{"logs", "-n", backupControllerNS, "-l", "app.kubernetes.io/component=controller", "-c", "topolvm-controller", "--tail=300"},
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

func testBackup() {
	if !isBackupEnabled() {
		return
	}

	var (
		ns      string
		bkpEnv  backupEnv
		applied bool
	)

	BeforeEach(func() {
		bkpEnv = loadBackupEnv()
		if !bkpEnv.complete() {
			Skip("BACKUP_* env vars not set; cannot run backup e2e")
		}
		// Per-run prefix keeps parallel or retried CI runs isolated inside the
		// shared bucket. If the caller (CI) set BACKUP_S3_PREFIX explicitly
		// (e.g. based on $GITHUB_RUN_ID) we honor it; otherwise fall back to a
		// locally-generated random prefix.
		if bkpEnv.prefix == "" {
			bkpEnv.prefix = "e2e/" + randomString()
		}
		ns = "backup-test-" + randomString()
		createNamespace(ns)
	})

	AfterEach(func() {
		if CurrentSpecReport().State.Is(types.SpecStateFailureStates) {
			dumpBackupDiagnostics(ns)
		}
		// Delete the BackupConfig first so the BackupConfig controller stops
		// reconciling PVCBackups while the namespace is torn down — otherwise
		// it may keep recreating objects in a doomed namespace.
		if applied {
			_, _ = kubectl("delete", "backupconfig", "default", "--ignore-not-found", "--wait=false")
		}
		_, err := kubectl("delete", "namespaces", ns, "--wait=true", "--timeout=120s")
		if err != nil {
			logf("namespace teardown did not complete cleanly: %v\n", err)
			dumpBackupDiagnostics(ns)
		}
	})

	It("backs up a PVC to restic and restores it into a fresh PVC", func() {
		By("creating S3 + restic password Secrets in the controller namespace")
		s3Yaml := fmt.Sprintf(backupS3CredsSecretYAML, backupS3CredsSecret, backupControllerNS, bkpEnv.accessKey, bkpEnv.secretKey)
		_, err := kubectlWithInput([]byte(s3Yaml), "apply", "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		pwYaml := fmt.Sprintf(backupResticPasswordSecretYAML, backupResticPwSecret, backupControllerNS, bkpEnv.resticPassword)
		_, err = kubectlWithInput([]byte(pwYaml), "apply", "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("applying the cluster-wide BackupConfig")
		bcYAML := fmt.Sprintf(backupConfigTemplateYAML,
			bkpEnv.endpoint, bkpEnv.bucket, bkpEnv.prefix,
			backupS3CredsSecret, backupControllerNS,
			backupResticPwSecret, backupControllerNS,
		)
		_, err = kubectlWithInput([]byte(bcYAML), "apply", "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())
		applied = true

		By("creating the source PVC and writing a sentinel file")
		pvcYAML := fmt.Sprintf(backupPVCTemplateYAML, backupPVCName, backupPVCSizeMi)
		_, err = kubectlWithInput([]byte(pvcYAML), "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		// The TopoLVM StorageClass uses WaitForFirstConsumer, so the PVC
		// stays Pending until a Pod references it. Create the writer Pod
		// right away and use its completion as the implicit bind check.
		writerYAML := fmt.Sprintf(backupWriterPodTemplateYAML, backupWriterPodName, backupFileContents, backupPVCName)
		_, err = kubectlWithInput([]byte(writerYAML), "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			var p corev1.Pod
			if err := getObjects(&p, "pod", "-n", ns, backupWriterPodName); err != nil {
				return err
			}
			if p.Status.Phase != corev1.PodSucceeded {
				return fmt.Errorf("writer pod phase=%s", p.Status.Phase)
			}
			return nil
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("waiting for the BackupConfig controller to create a managed PVCBackup")
		Eventually(func() error {
			_, err := kubectl("get", "pvcbackup", "-n", ns, backupPVCName)
			return err
		}).WithTimeout(1 * time.Minute).Should(Succeed())

		By("triggering an immediate backup run via .spec.trigger")
		trigger := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
		patch := fmt.Sprintf(`{"spec":{"trigger":%q}}`, trigger)
		_, err = kubectl("patch", "pvcbackup", "-n", ns, backupPVCName, "--type=merge", "-p", patch)
		Expect(err).ShouldNot(HaveOccurred())

		By("waiting for the PVCBackup to reach Synced")
		Eventually(func() (string, error) {
			out, err := kubectl("get", "pvcbackup", "-n", ns, backupPVCName, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(Equal("Synced"))

		By("deleting the source pod + PVC so the restore is against a clean slate")
		_, _ = kubectl("delete", "pod", "-n", ns, backupWriterPodName, "--ignore-not-found", "--wait=true")
		_, _ = kubectl("delete", "pvcbackup", "-n", ns, backupPVCName, "--wait=true")
		_, _ = kubectl("delete", "pvc", "-n", ns, backupPVCName, "--wait=true")

		By("creating a Restore CR targeting a fresh PVC")
		restoreYAML := fmt.Sprintf(backupRestoreTemplateYAML,
			backupRestoreName, ns,
			ns, backupPVCName,
			backupRestorePVCName, backupPVCSizeMi,
		)
		_, err = kubectlWithInput([]byte(restoreYAML), "apply", "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("waiting for the Restore to reach Succeeded")
		Eventually(func() (string, error) {
			out, err := kubectl("get", "restore", "-n", ns, backupRestoreName, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(Equal("Succeeded"))

		By("mounting the restored PVC and verifying the sentinel file")
		readerYAML := fmt.Sprintf(backupReaderPodTemplateYAML, backupReaderPodName, backupRestorePVCName)
		_, err = kubectlWithInput([]byte(readerYAML), "apply", "-n", ns, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			var p corev1.Pod
			if err := getObjects(&p, "pod", "-n", ns, backupReaderPodName); err != nil {
				return err
			}
			if p.Status.Phase != corev1.PodSucceeded {
				return fmt.Errorf("reader pod phase=%s", p.Status.Phase)
			}
			return nil
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		out, err := kubectl("logs", "-n", ns, backupReaderPodName)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(strings.TrimSpace(string(out))).To(Equal(backupFileContents),
			"restored PVC did not contain the pre-backup sentinel file")
	})
}
