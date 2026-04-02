package provider

import (
	"context"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// getOwnerDeploymentName returns the Deployment name for a pod owned via
// ReplicaSet, or "" if not a Deployment-owned pod.
func (p *CocoonProvider) getOwnerDeploymentName(ctx context.Context, pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "ReplicaSet" {
			continue
		}
		rsName := ref.Name
		idx := strings.LastIndex(rsName, "-")
		if idx <= 0 {
			break
		}
		candidate := rsName[:idx]
		rs, err := p.kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			break
		}
		for _, ownerRef := range rs.OwnerReferences {
			if ownerRef.Kind == "Deployment" {
				return candidate
			}
		}
		break
	}
	return ""
}

// getOwnerStatefulSetName returns the StatefulSet name if the pod is owned by one.
func getOwnerStatefulSetName(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "StatefulSet" {
			return ref.Name
		}
	}
	return ""
}

// extractOrdinal extracts the ordinal index from a StatefulSet pod name.
// e.g. "agent-group-2" -> 2, "bot-0" -> 0.
func extractOrdinal(podName string) int {
	if idx := strings.LastIndex(podName, "-"); idx >= 0 {
		if n, err := strconv.Atoi(podName[idx+1:]); err == nil {
			return n
		}
	}
	return -1
}

// getDesiredReplicas queries the desired replica count for a Deployment or StatefulSet.
// Returns -1 if the resource is not found (deleted) or the query fails.
func (p *CocoonProvider) getDesiredReplicas(ctx context.Context, ns, kind, name string) int {
	if kind == "StatefulSet" {
		sts, err := p.kubeClient.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return -1
		}
		if sts.Spec.Replicas == nil {
			return -1
		}
		return int(*sts.Spec.Replicas)
	}
	deploy, err := p.kubeClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return -1
	}
	if deploy.Spec.Replicas == nil {
		return -1
	}
	return int(*deploy.Spec.Replicas)
}

// countDeploymentPods counts pods tracked by the provider that belong to the
// given Deployment (matched by ReplicaSet owner ref prefix).
func (p *CocoonProvider) countDeploymentPods(ns, deployName string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, pod := range p.pods {
		if pod.Namespace != ns {
			continue
		}
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "ReplicaSet" {
				if idx := strings.LastIndex(ref.Name, "-"); idx > 0 && ref.Name[:idx] == deployName {
					count++
				}
			}
		}
	}
	return count
}
