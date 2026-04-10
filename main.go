// vk-cocoon is the cocoonstack virtual-kubelet provider that maps
// Kubernetes pods to cocoon MicroVMs. It runs as a host-level
// systemd service alongside the cocoon CLI and a single
// cloud-hypervisor instance per VM.
//
// This file is the binary entry point. The actual provider lives in
// provider.go and the per-feature files alongside it; the supporting
// subpackages (vm, snapshots, network, guest, probes, metrics) carry
// the cocoon CLI wrapper, the epoch SDK adapter, the dnsmasq lease
// parser, the SSH/RDP exec layer, the probe loop, and the
// prometheus collectors respectively.
package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/projecteru2/core/log"

	commonlog "github.com/cocoonstack/cocoon-common/log"
	"github.com/cocoonstack/vk-cocoon/version"
)

func main() {
	ctx := context.Background()
	commonlog.Setup(ctx, "VK_LOG_LEVEL")

	logger := log.WithFunc("main")
	logger.Infof(ctx, "vk-cocoon %s starting (rev=%s built=%s)",
		version.VERSION, version.REVISION, version.BUILTAT)

	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Subsequent commits replace this with the full virtual-kubelet
	// node bootstrap: kubeconfig + clientset, TLS for the kubelet
	// API, the CocoonProvider, the startup reconcile, and the
	// event-driven loop.
	<-signalCtx.Done()
	logger.Infof(signalCtx, "vk-cocoon exiting")
}
