package provider

import (
	"fmt"
	"os/exec"
)

// addPodDNS adds a dnsmasq host entry for the pod: pod-name -> VM IP.
// Pods can resolve each other by name within the cocoon bridge.
func addPodDNS(podName, namespace, ip string) {
	if ip == "" {
		return
	}
	entry := fmt.Sprintf("%s\t%s %s.%s.svc.cluster.local", ip, podName, podName, namespace)
	cmd := fmt.Sprintf("grep -q '%s' /etc/hosts 2>/dev/null || echo '%s' >> /etc/hosts", podName, entry)
	_, _ = exec.Command("sudo", "bash", "-c", cmd).CombinedOutput() //nolint:gosec // cmd from trusted template
}

// removePodDNS removes the dnsmasq host entry for the pod.
func removePodDNS(podName string) {
	cmd := fmt.Sprintf("sudo sed -i '/%s/d' /etc/hosts 2>/dev/null", podName)
	_ = exec.Command("bash", "-c", cmd).Run() //nolint:gosec
}
