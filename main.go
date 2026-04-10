// vk-cocoon is the cocoonstack virtual-kubelet provider that maps
// Kubernetes pods to cocoon MicroVMs. It runs as a host-level
// systemd service alongside the cocoon CLI and a single
// cloud-hypervisor instance per VM.
//
// This file is the binary entry point. The provider lives in
// provider.go and the per-feature files alongside it (pods_create,
// pods_delete, pods_update, pods_status, access, reconcile);
// the supporting subpackages (vm, snapshots, network, guest,
// probes, metrics) carry the cocoon CLI wrapper, the epoch SDK
// adapter, the dnsmasq lease parser, the SSH/RDP exec layer, the
// probe loop, and the prometheus collectors respectively.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	commonlog "github.com/cocoonstack/cocoon-common/log"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/version"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	defaultNodeName     = "cocoon-pool"
	defaultMetricsAddr  = ":9091"
	defaultEpochURL     = "http://epoch.cocoon-system.svc:8080"
	defaultLeasesPath   = "/var/lib/dnsmasq/dnsmasq.leases"
	defaultSSHUser      = "root"
	defaultSSHPort      = 22
	defaultOrphanPolicy = string(OrphanAlert)
)

func main() {
	ctx := context.Background()
	commonlog.Setup(ctx, "VK_LOG_LEVEL")

	logger := log.WithFunc("main")
	logger.Infof(ctx, "vk-cocoon %s starting (rev=%s built=%s)",
		version.VERSION, version.REVISION, version.BUILTAT)

	nodeName := envOrDefault("VK_NODE_NAME", defaultNodeName)
	metricsAddr := envOrDefault("VK_METRICS_ADDR", defaultMetricsAddr)
	epochURL := envOrDefault("EPOCH_URL", defaultEpochURL)
	epochToken := os.Getenv("EPOCH_TOKEN")
	leasesPath := envOrDefault("VK_LEASES_PATH", defaultLeasesPath)
	cocoonBin := envOrDefault("VK_COCOON_BIN", "")
	sshPassword := os.Getenv("VK_SSH_PASSWORD")
	orphanPolicy := envOrDefault("VK_ORPHAN_POLICY", defaultOrphanPolicy)

	// Build the K8s clientset.
	cfg, err := commonk8s.LoadConfig()
	if err != nil {
		logger.Fatalf(ctx, err, "load kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf(ctx, err, "build clientset: %v", err)
	}

	metrics.Register(prometheus.DefaultRegisterer)

	// Wire the provider.
	registry := snapshots.New(epochURL, epochToken)
	runtime := vm.NewCocoonCLI(cocoonBin, true)
	puller := &snapshots.Puller{Registry: registry, Runtime: runtime}
	pusher := &snapshots.Pusher{Registry: registry, Runtime: runtime}

	provider := NewCocoonProvider()
	provider.NodeName = nodeName
	provider.Clientset = clientset
	provider.Runtime = runtime
	provider.Puller = puller
	provider.Pusher = pusher
	provider.Registry = registry
	provider.LeaseParser = network.NewLeaseParser(leasesPath)
	provider.GuestSSH = guest.NewSSHExecutor(defaultSSHUser, sshPassword, defaultSSHPort)
	provider.GuestRDP = guest.RDPExecutor{}
	provider.Probes = probes.NewManager()
	provider.OrphanPolicy = OrphanPolicy(strings.ToLower(orphanPolicy))

	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := provider.StartupReconcile(signalCtx); err != nil {
		// Refusing to register the v-k node is the safe default:
		// continuing with empty pod / VM tables would make every
		// live cocoon VM look like an orphan on the next reconcile
		// and would have GetPodStatus 404 every pod the scheduler
		// already placed. systemd will restart us so transient
		// API-server hiccups still recover.
		logger.Fatalf(signalCtx, err, "startup reconcile failed; refusing to register node")
	}

	// Plain-HTTP metrics listener (kubelet TLS lives elsewhere).
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Infof(signalCtx, "vk-cocoon metrics listening on %s", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf(signalCtx, err, "metrics listen and serve")
		}
	}()

	// Subsequent commits register the CocoonProvider with the
	// virtual-kubelet node controller and start serving the
	// kubelet API. For the rewrite milestone the binary blocks on
	// the signal context so the lifecycle stays observable in dev.
	<-signalCtx.Done()

	shutdownCtx := context.Background()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Warnf(shutdownCtx, "shutdown metrics: %v", err)
	}
	logger.Infof(shutdownCtx, "vk-cocoon exiting")
}

// envOrDefault returns the value of key from the environment, or
// fallback when key is unset or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
