package nfs

import (
	"fmt"

	"github.com/topolvm/topolvm"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

type Config struct {
	UserPVCNamespace    string
	UserPVCName         string
	UserPVCUID          string
	Size                resource.Quantity
	BackingStorageClass string
	GaneshaImage        string
	DataSource          *corev1.TypedLocalObjectReference
}

func BackingPVCName(userPVCName string) string {
	return userPVCName + topolvm.RWXBackingPVCSuffix
}

func ServerName(userPVCName string) string {
	return userPVCName + topolvm.RWXNFSServerSuffix
}

func ConfigMapName(userPVCName string) string {
	return userPVCName + topolvm.RWXNFSServerSuffix + "-config"
}

func PVName(userPVCNamespace, userPVCName string) string {
	return fmt.Sprintf("%s-%s%s", userPVCNamespace, userPVCName, topolvm.RWXPVSuffix)
}

func commonLabels(cfg Config) map[string]string {
	return map[string]string{
		topolvm.CreatedbyLabelKey:         topolvm.CreatedbyLabelValue,
		topolvm.RWXManagedByLabel:         "topolvm-rwx",
		topolvm.RWXOwnerPVCNamespaceLabel: cfg.UserPVCNamespace,
		topolvm.RWXOwnerPVCNameLabel:      cfg.UserPVCName,
	}
}

const ganeshaConfig = `NFS_CORE_PARAM {
    Protocols = 4;
    NFS_Port = 2049;
}

NFSv4 {
    Grace_Period = 10;
}

EXPORT {
    Export_Id = 1;
    Path = /export;
    Pseudo = /export;
    Access_Type = RW;
    Squash = No_Root_Squash;
    SecType = sys;
    Protocols = 4;
    Transports = TCP;
    FSAL { Name = VFS; }
}
`

func ConfigMap(cfg Config) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName(cfg.UserPVCName),
			Namespace: cfg.UserPVCNamespace,
			Labels:    commonLabels(cfg),
		},
		Data: map[string]string{
			"ganesha.conf": ganeshaConfig,
		},
	}
}

func BackingPVC(cfg Config) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BackingPVCName(cfg.UserPVCName),
			Namespace: cfg.UserPVCNamespace,
			Labels:    commonLabels(cfg),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(cfg.BackingStorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: cfg.Size,
				},
			},
		},
	}
	if cfg.DataSource != nil {
		pvc.Spec.DataSource = cfg.DataSource
	}
	return pvc
}

func Deployment(cfg Config) *appsv1.Deployment {
	image := cfg.GaneshaImage
	if image == "" {
		image = topolvm.DefaultGaneshaImage
	}
	name := ServerName(cfg.UserPVCName)
	labels := commonLabels(cfg)
	selector := map[string]string{
		"app.kubernetes.io/name":     "topolvm-rwx-nfs",
		"app.kubernetes.io/instance": name,
	}
	for k, v := range selector {
		labels[k] = v
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.UserPVCNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "nfs-ganesha",
						Image:   image,
						Command: []string{"/usr/bin/ganesha.nfsd"},
						Args:    []string{"-F", "-L", "/dev/stdout", "-f", "/etc/ganesha/ganesha.conf"},
						SecurityContext: &corev1.SecurityContext{
							Privileged:               ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(false),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add: []corev1.Capability{
									"SYS_ADMIN",
									"DAC_READ_SEARCH",
									"SETPCAP",
									"CHOWN",
									"FOWNER",
									"SETUID",
									"SETGID",
								},
							},
						},
						Ports: []corev1.ContainerPort{
							{Name: "nfs", ContainerPort: 2049, Protocol: corev1.ProtocolTCP},
							{Name: "mountd", ContainerPort: 20048, Protocol: corev1.ProtocolTCP},
							{Name: "rpcbind", ContainerPort: 111, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "export", MountPath: "/export"},
							{Name: "ganesha-config", MountPath: "/etc/ganesha"},
							{Name: "ganesha-state", MountPath: "/var/lib/nfs/ganesha"},
							{Name: "dbus", MountPath: "/var/run/dbus"},
						},
						ReadinessProbe: &corev1.Probe{
							InitialDelaySeconds: 10,
							PeriodSeconds:       10,
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt(2049),
								},
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "export",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: BackingPVCName(cfg.UserPVCName),
								},
							},
						},
						{
							Name: "ganesha-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: ConfigMapName(cfg.UserPVCName),
									},
								},
							},
						},
						{
							Name:         "ganesha-state",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							Name:         "dbus",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

func Service(cfg Config) *corev1.Service {
	name := ServerName(cfg.UserPVCName)
	labels := commonLabels(cfg)
	selector := map[string]string{
		"app.kubernetes.io/name":     "topolvm-rwx-nfs",
		"app.kubernetes.io/instance": name,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.UserPVCNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{
				{Name: "nfs", Port: 2049, TargetPort: intstr.FromInt(2049), Protocol: corev1.ProtocolTCP},
				{Name: "mountd", Port: 20048, TargetPort: intstr.FromInt(20048), Protocol: corev1.ProtocolTCP},
				{Name: "rpcbind", Port: 111, TargetPort: intstr.FromInt(111), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func PV(cfg Config, serviceClusterIP string) *corev1.PersistentVolume {
	pvName := PVName(cfg.UserPVCNamespace, cfg.UserPVCName)
	server := fmt.Sprintf("%s.%s.svc.cluster.local", ServerName(cfg.UserPVCName), cfg.UserPVCNamespace)
	if serviceClusterIP != "" {
		server = serviceClusterIP
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   pvName,
			Labels: commonLabels(cfg),
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: cfg.Size,
			},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions: []string{
				"nfsvers=4.1",
				"hard",
				"noatime",
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       topolvm.NFSCSIDriverName,
					VolumeHandle: pvName,
					VolumeAttributes: map[string]string{
						"server": server,
						"share":  "/export",
					},
				},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind:       "PersistentVolumeClaim",
				Namespace:  cfg.UserPVCNamespace,
				Name:       cfg.UserPVCName,
				UID:        types.UID(cfg.UserPVCUID),
				APIVersion: "v1",
			},
			StorageClassName: "",
		},
	}
}
