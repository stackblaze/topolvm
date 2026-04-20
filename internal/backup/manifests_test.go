package backup

import (
	"strings"
	"testing"

	"github.com/topolvm/topolvm"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func baseCfg() RuntimeConfig {
	return RuntimeConfig{
		PVCNamespace:             "app",
		PVCName:                  "data",
		PVCSize:                  resource.MustParse("10Gi"),
		StorageClassName:         "topolvm-provisioner",
		PVCBackupName:            "data",
		PVCBackupUID:             "pb-uid-1",
		VolumeSnapshotClassName:  "topolvm-provisioner",
		KeepSnapshotAfterBackup:  false,
		S3Endpoint:               "https://s3.example.com/",
		S3Bucket:                 "backups",
		S3Region:                 "us-east-1",
		S3CredentialsSecretName:  "s3-creds",
		ResticPasswordSecretName: "restic-pw",
		ResticImage:              "restic/restic:0.17.3",
		ServiceAccountName:       "topolvm-backup-mover",
	}
}

func TestRepoURL_StripsTrailingSlash(t *testing.T) {
	got := RepoURL("https://s3.example.com/", "backups", "", "app", "data")
	want := "s3:https://s3.example.com/backups/app/data"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRepoURL_PerPVCLayout(t *testing.T) {
	// Two different PVCs must resolve to distinct prefixes.
	a := RepoURL("https://s3", "bucket", "", "ns1", "pvc1")
	b := RepoURL("https://s3", "bucket", "", "ns1", "pvc2")
	c := RepoURL("https://s3", "bucket", "", "ns2", "pvc1")
	if a == b || a == c || b == c {
		t.Fatalf("expected distinct repo URLs, got a=%s b=%s c=%s", a, b, c)
	}
	if !strings.HasSuffix(a, "/ns1/pvc1") {
		t.Fatalf("expected <ns>/<name> suffix, got %s", a)
	}
}

func TestRepoURL_PrefixInsertedBetweenBucketAndNamespace(t *testing.T) {
	got := RepoURL("https://s3", "bucket", "ci/42", "ns", "data")
	want := "s3:https://s3/bucket/ci/42/ns/data"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Leading/trailing slashes on prefix must be stripped to avoid double-/ paths.
	if got := RepoURL("https://s3", "bucket", "/ci/42/", "ns", "data"); got != want {
		t.Fatalf("prefix with surrounding slashes produced %q, want %q", got, want)
	}
}

func TestNames_AreDeterministicAndDistinct(t *testing.T) {
	const runID = "20260419120000"
	snap := SnapshotName("data", runID)
	tpvc := TempPVCName("data", runID)
	job := MoverJobName("data", runID)
	restore := RestoreJobName("my-restore")

	for _, n := range []string{snap, tpvc, job, restore} {
		if n == "" {
			t.Fatalf("empty name")
		}
	}
	if snap == tpvc || snap == job || tpvc == job {
		t.Fatalf("expected distinct names, got snap=%s tpvc=%s job=%s", snap, tpvc, job)
	}
	if !strings.HasSuffix(snap, runID) || !strings.HasSuffix(tpvc, runID) || !strings.HasSuffix(job, runID) {
		t.Fatalf("names must carry the runID suffix for deterministic regeneration")
	}
	if !strings.HasSuffix(restore, topolvm.RestoreJobSuffix) {
		t.Fatalf("restore job name missing expected suffix: %s", restore)
	}
}

func TestSnapshot_HasOwnerRefAndLabels(t *testing.T) {
	cfg := baseCfg()
	snap := Snapshot(cfg, "run1")

	if got := len(snap.OwnerReferences); got != 1 {
		t.Fatalf("expected 1 owner ref, got %d", got)
	}
	or := snap.OwnerReferences[0]
	if or.Kind != "PVCBackup" || or.Name != "data" || string(or.UID) != "pb-uid-1" {
		t.Fatalf("owner ref mismatch: %+v", or)
	}
	if or.Controller == nil || !*or.Controller {
		t.Fatalf("owner ref must be Controller=true")
	}
	if snap.Spec.Source.PersistentVolumeClaimName == nil || *snap.Spec.Source.PersistentVolumeClaimName != "data" {
		t.Fatalf("snapshot source PVC name wrong")
	}
	if snap.Spec.VolumeSnapshotClassName == nil || *snap.Spec.VolumeSnapshotClassName != "topolvm-provisioner" {
		t.Fatalf("snapshot class not propagated")
	}
	if snap.Labels[topolvm.BackupManagedByLabel] != topolvm.BackupManagedByValue {
		t.Fatalf("missing managed-by label")
	}
}

func TestSnapshot_EmptyClassIsNil(t *testing.T) {
	cfg := baseCfg()
	cfg.VolumeSnapshotClassName = ""
	snap := Snapshot(cfg, "run1")
	if snap.Spec.VolumeSnapshotClassName != nil {
		t.Fatalf("expected nil snapshot class when empty, got %q", *snap.Spec.VolumeSnapshotClassName)
	}
}

func TestTempPVC_UsesSnapshotDataSource(t *testing.T) {
	cfg := baseCfg()
	pvc := TempPVC(cfg, "run1")

	if pvc.Spec.DataSource == nil {
		t.Fatal("temp PVC missing dataSource")
	}
	if pvc.Spec.DataSource.Kind != "VolumeSnapshot" {
		t.Fatalf("expected VolumeSnapshot dataSource, got %s", pvc.Spec.DataSource.Kind)
	}
	if pvc.Spec.DataSource.Name != SnapshotName(cfg.PVCName, "run1") {
		t.Fatalf("dataSource name must match Snapshot() output")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != cfg.StorageClassName {
		t.Fatalf("temp PVC did not inherit source storage class")
	}
	if pvc.Spec.Resources.Requests["storage"] != cfg.PVCSize {
		t.Fatalf("temp PVC size mismatch")
	}
}

func TestMoverJob_MountsTempPVCReadOnly(t *testing.T) {
	cfg := baseCfg()
	job := MoverJob(cfg, "run1")

	if got := len(job.Spec.Template.Spec.Volumes); got != 1 {
		t.Fatalf("expected 1 volume, got %d", got)
	}
	vol := job.Spec.Template.Spec.Volumes[0]
	if vol.PersistentVolumeClaim == nil {
		t.Fatal("volume must be a PVC source")
	}
	if vol.PersistentVolumeClaim.ClaimName != TempPVCName(cfg.PVCName, "run1") {
		t.Fatalf("mover must mount the temp PVC from the same runID")
	}
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Fatal("mover must mount the temp PVC read-only to avoid mutating a snapshot copy")
	}
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/data" || !mounts[0].ReadOnly {
		t.Fatalf("unexpected volumeMounts: %+v", mounts)
	}
}

func TestMoverJob_BackoffLimitZero(t *testing.T) {
	// The reconciler drives retries itself — the Job must not retry in-pod.
	job := MoverJob(baseCfg(), "run1")
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("expected BackoffLimit=0, got %+v", job.Spec.BackoffLimit)
	}
}

func TestMoverEnv_SecretKeysMatchConstants(t *testing.T) {
	env := moverEnv(baseCfg())

	want := map[string]string{
		"RESTIC_PASSWORD":       topolvm.ResticPasswordSecretKey,
		"AWS_ACCESS_KEY_ID":     topolvm.S3AccessKeyIDSecretKey,
		"AWS_SECRET_ACCESS_KEY": topolvm.S3SecretKeySecretKey,
	}
	seen := map[string]string{}
	for _, e := range env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			seen[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}
	for envName, expectedKey := range want {
		if got, ok := seen[envName]; !ok {
			t.Errorf("env %s missing SecretKeyRef", envName)
		} else if got != expectedKey {
			t.Errorf("env %s keyRef=%s, want %s", envName, got, expectedKey)
		}
	}
}

func TestMoverEnv_RetentionFlagsPropagated(t *testing.T) {
	cfg := baseCfg()
	cfg.Retention = topolvmv1.RetentionSpec{
		KeepLast:    3,
		KeepDaily:   7,
		KeepMonthly: 0,
	}
	env := moverEnv(cfg)

	values := map[string]string{}
	for _, e := range env {
		values[e.Name] = e.Value
	}
	if values["KEEP_LAST"] != "3" {
		t.Errorf("KEEP_LAST=%q, want 3", values["KEEP_LAST"])
	}
	if values["KEEP_DAILY"] != "7" {
		t.Errorf("KEEP_DAILY=%q, want 7", values["KEEP_DAILY"])
	}
	if values["KEEP_MONTHLY"] != "" {
		t.Errorf("KEEP_MONTHLY=%q, want empty (0 retention -> unset)", values["KEEP_MONTHLY"])
	}
}

func TestMoverEnv_RegionOnlyWhenSet(t *testing.T) {
	cfg := baseCfg()
	cfg.S3Region = ""
	for _, e := range moverEnv(cfg) {
		if e.Name == "AWS_DEFAULT_REGION" {
			t.Fatal("AWS_DEFAULT_REGION should not be set when S3Region is empty")
		}
	}
	cfg.S3Region = "eu-west-1"
	found := false
	for _, e := range moverEnv(cfg) {
		if e.Name == "AWS_DEFAULT_REGION" && e.Value == "eu-west-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("AWS_DEFAULT_REGION not set despite S3Region=eu-west-1")
	}
}

func TestRestoreJob_OwnedByRestoreNotTargetPVC(t *testing.T) {
	cfg := RestoreConfig{
		RestoreName:              "rs1",
		RestoreNamespace:         "app",
		RestoreUID:               "rs-uid",
		TargetPVCName:            "restored",
		SourceNamespace:          "app",
		SourcePVCName:            "data",
		S3Endpoint:               "https://s3",
		S3Bucket:                 "b",
		S3CredentialsSecretName:  "s3-creds",
		ResticPasswordSecretName: "restic-pw",
		ResticImage:              "restic/restic:0.17.3",
		ServiceAccountName:       "default",
	}
	job := RestoreJob(cfg)

	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "Restore" {
		t.Fatalf("restore Job must be owned by the Restore CR, got %+v", job.OwnerReferences)
	}
	// Target PVC is mounted but NOT owned by the Job — restored data must
	// survive deletion of the Restore CR (and therefore the Job).
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != cfg.TargetPVCName {
			t.Fatalf("restore Job must mount target PVC, got %s", v.PersistentVolumeClaim.ClaimName)
		}
	}
}

func TestRestoreJob_DefaultSnapshotIDIsLatest(t *testing.T) {
	cfg := RestoreConfig{
		RestoreName:              "rs1",
		RestoreNamespace:         "app",
		RestoreUID:               "rs-uid",
		TargetPVCName:            "restored",
		SourceNamespace:          "app",
		SourcePVCName:            "data",
		S3Endpoint:               "https://s3",
		S3Bucket:                 "b",
		S3CredentialsSecretName:  "s3-creds",
		ResticPasswordSecretName: "restic-pw",
		ResticImage:              "restic/restic:0.17.3",
		ServiceAccountName:       "default",
		// SnapshotID left empty — should resolve to "latest".
	}
	job := RestoreJob(cfg)
	var snapEnv string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SNAPSHOT_ID" {
			snapEnv = e.Value
		}
	}
	if snapEnv != "latest" {
		t.Fatalf("default SnapshotID should be latest, got %q", snapEnv)
	}
}
