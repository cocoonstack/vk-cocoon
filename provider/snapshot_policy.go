package provider

import (
	"context"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

// shouldSnapshotOnDelete determines whether a pod's VM should be snapshotted
// and pushed to epoch before destruction. Returns false for scale-down (the
// replica slot is being permanently removed), true for pod restart / kill /
// deployment suspend (replicas=0).
func (p *CocoonProvider) shouldSnapshotOnDelete(ctx context.Context, pod *corev1.Pod) bool {
	logger := log.WithFunc("provider.shouldSnapshotOnDelete")
	key := podKey(pod.Namespace, pod.Name)

	if policy := ann(pod, AnnSnapshotPolicy, ""); policy != "" {
		switch policy {
		case "never":
			logger.Infof(ctx, "%s: annotation snapshot-policy=never", key)
			return false
		case "always":
			logger.Infof(ctx, "%s: annotation snapshot-policy=always", key)
			return true
		case "main-only":
			vm := p.getVM(pod.Namespace, pod.Name)
			if vm != nil && isMainAgent(vm.vmName) {
				logger.Infof(ctx, "%s: annotation snapshot-policy=main-only (is main)", key)
				return true
			}
			logger.Infof(ctx, "%s: annotation snapshot-policy=main-only (not main, skip)", key)
			return false
		}
	}

	if ann(pod, AnnHibernate, "") == valTrue {
		logger.Infof(ctx, "%s: hibernate annotation set, snapshot", key)
		return true
	}

	if stsName := getOwnerStatefulSetName(pod); stsName != "" {
		desired := p.getDesiredReplicas(ctx, pod.Namespace, "StatefulSet", stsName)
		if desired <= 0 {
			return true
		}
		ordinal := extractOrdinal(pod.Name)
		if ordinal >= 0 && ordinal >= desired {
			logger.Infof(ctx, "%s: StatefulSet scale-down (ordinal=%d >= desired=%d), skip", key, ordinal, desired)
			return false
		}
		return true
	}

	if deployName := p.getOwnerDeploymentName(ctx, pod); deployName != "" {
		desired := p.getDesiredReplicas(ctx, pod.Namespace, "Deployment", deployName)
		if desired <= 0 {
			return true
		}
		current := p.countDeploymentPods(pod.Namespace, deployName)
		if current > desired {
			logger.Infof(ctx, "%s: Deployment scale-down (current=%d > desired=%d), skip", key, current, desired)
			return false
		}
		return true
	}

	return true
}
