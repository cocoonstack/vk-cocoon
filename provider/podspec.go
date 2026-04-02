package provider

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	defaultImageName      = "openclaw-agent-golden-v2"
	defaultLinuxStorage   = "100G"
	defaultWindowsStorage = "15G"
	defaultNICs           = "1"
	defaultOSType         = "linux"
)

// podSpecResolution captures the pod-level values we repeatedly derive from
// annotations and the first container spec.
type podSpecResolution struct {
	imageRaw    string
	registryURL string
	image       string
	storage     string
	nics        string
	dns         string
	rootPwd     string
	osType      string
}

func resolvePodSpec(pod *corev1.Pod) podSpecResolution {
	imageRaw := ann(pod, AnnImage, "")
	if imageRaw == "" && len(pod.Spec.Containers) > 0 {
		imageRaw = pod.Spec.Containers[0].Image
	}
	if imageRaw == "" {
		imageRaw = defaultImageName
	}
	registryURL, image := parseImageRef(imageRaw)
	osType := ann(pod, AnnOS, defaultOSType)

	return podSpecResolution{
		imageRaw:    imageRaw,
		registryURL: registryURL,
		image:       image,
		storage:     ann(pod, AnnStorage, defaultStorageForOS(osType)),
		nics:        ann(pod, AnnNICs, defaultNICs),
		dns:         ann(pod, AnnDNS, ""),
		rootPwd:     ann(pod, AnnRootPassword, ""),
		osType:      osType,
	}
}

// runImage returns the direct image reference used by run mode.
func (r podSpecResolution) runImage() string {
	return r.imageRaw
}

// cloneImage returns the snapshot/image name used by clone mode.
func (r podSpecResolution) cloneImage() string {
	return r.image
}

func defaultStorageForOS(osType string) string {
	if osType == osWindows {
		return defaultWindowsStorage
	}
	return defaultLinuxStorage
}
