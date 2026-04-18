package nfs

import (
	"testing"

	"github.com/topolvm/topolvm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func sampleConfig() Config {
	return Config{
		UserPVCNamespace:    "default",
		UserPVCName:         "data",
		UserPVCUID:          "11111111-2222-3333-4444-555555555555",
		Size:                resource.MustParse("5Gi"),
		BackingStorageClass: "topolvm-provisioner",
		GaneshaImage:        "",
	}
}

func TestBackingPVC(t *testing.T) {
	pvc := BackingPVC(sampleConfig())
	if pvc.Name != "data"+topolvm.RWXBackingPVCSuffix {
		t.Errorf("unexpected name: %s", pvc.Name)
	}
	if pvc.Namespace != "default" {
		t.Errorf("unexpected namespace: %s", pvc.Namespace)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("backing PVC must be RWO, got %v", pvc.Spec.AccessModes)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "topolvm-provisioner" {
		t.Errorf("backing PVC storage class mismatch: %v", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
		t.Errorf("backing PVC size mismatch: %s", got.String())
	}
}

func TestDeploymentDefaultsToBundledImage(t *testing.T) {
	dep := Deployment(sampleConfig())
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != topolvm.DefaultGaneshaImage {
		t.Errorf("expected default Ganesha image, got %s", got)
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("single replica expected, got %d", *dep.Spec.Replicas)
	}
	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].PersistentVolumeClaim == nil {
		t.Fatalf("deployment must mount backing PVC, got %+v", vols)
	}
	if vols[0].PersistentVolumeClaim.ClaimName != "data"+topolvm.RWXBackingPVCSuffix {
		t.Errorf("deployment must reference backing PVC, got %s", vols[0].PersistentVolumeClaim.ClaimName)
	}
}

func TestDeploymentIsNotPrivileged(t *testing.T) {
	dep := Deployment(sampleConfig())
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("expected a SecurityContext, got nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Errorf("expected Privileged=false, got %v", sc.Privileged)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("expected AllowPrivilegeEscalation=false, got %v", sc.AllowPrivilegeEscalation)
	}
	if sc.Capabilities == nil {
		t.Fatal("expected Capabilities block, got nil")
	}
	drops := map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Drop {
		drops[c] = true
	}
	if !drops["ALL"] {
		t.Errorf("expected capabilities drop ALL, got %v", sc.Capabilities.Drop)
	}
	adds := map[corev1.Capability]bool{}
	for _, c := range sc.Capabilities.Add {
		adds[c] = true
	}
	for _, want := range []corev1.Capability{"SYS_ADMIN", "DAC_READ_SEARCH", "CHOWN"} {
		if !adds[want] {
			t.Errorf("expected capability %q to be added, got %v", want, sc.Capabilities.Add)
		}
	}
}

func TestDeploymentImageOverride(t *testing.T) {
	cfg := sampleConfig()
	cfg.GaneshaImage = "my.registry/ganesha:custom"
	dep := Deployment(cfg)
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "my.registry/ganesha:custom" {
		t.Errorf("image override not applied, got %s", got)
	}
}

func TestServiceExposesNFSPorts(t *testing.T) {
	svc := Service(sampleConfig())
	have := map[int32]bool{}
	for _, p := range svc.Spec.Ports {
		have[p.Port] = true
	}
	for _, want := range []int32{2049, 111, 20048} {
		if !have[want] {
			t.Errorf("service missing port %d", want)
		}
	}
}

func TestPVPrefersClusterIP(t *testing.T) {
	pv := PV(sampleConfig(), "10.96.0.123")
	if pv.Spec.CSI.VolumeAttributes["server"] != "10.96.0.123" {
		t.Errorf("PV server should prefer ClusterIP, got %q", pv.Spec.CSI.VolumeAttributes["server"])
	}
	if pv.Spec.CSI.Driver != topolvm.NFSCSIDriverName {
		t.Errorf("PV must use NFS CSI driver, got %s", pv.Spec.CSI.Driver)
	}
	if len(pv.Spec.AccessModes) != 1 || pv.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("PV must be RWX, got %v", pv.Spec.AccessModes)
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != "data" {
		t.Errorf("PV ClaimRef must bind to user PVC, got %+v", pv.Spec.ClaimRef)
	}
}

func TestPVFallsBackToServiceDNS(t *testing.T) {
	pv := PV(sampleConfig(), "")
	wantServer := "data" + topolvm.RWXNFSServerSuffix + ".default.svc.cluster.local"
	if got := pv.Spec.CSI.VolumeAttributes["server"]; got != wantServer {
		t.Errorf("expected DNS fallback %q, got %q", wantServer, got)
	}
}
