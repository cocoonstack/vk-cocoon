// vk-cocoon is the virtual-kubelet provider that maps Kubernetes pods to cocoon MicroVMs.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	commonlog "github.com/cocoonstack/cocoon-common/log"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/provider/cocoon"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/version"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	defaultNodeName     = "cocoon-pool"
	defaultMetricsAddr  = ":9091"
	defaultEpochURL     = "http://epoch.cocoon-system.svc:8080"
	defaultSSHUser      = "root"
	defaultSSHPort      = 22
	defaultOrphanPolicy = string(provider.OrphanAlert)

	defaultTLSCert     = "/etc/cocoon/vk/tls/vk-kubelet.crt"
	defaultTLSKey      = "/etc/cocoon/vk/tls/vk-kubelet.key"
	kubeletAPIPort     = 10250
	endpointPatchWait  = 5 * time.Second
	endpointPatchRetry = 2 * time.Second
)

func main() {
	ctx := context.Background()
	commonlog.Setup(ctx, "VK_LOG_LEVEL")

	logger := log.WithFunc("main")
	logger.Infof(ctx, "vk-cocoon %s starting (rev=%s built=%s)",
		version.VERSION, version.REVISION, version.BUILTAT)

	nodeName := commonk8s.EnvOrDefault("VK_NODE_NAME", defaultNodeName)
	metricsAddr := commonk8s.EnvOrDefault("VK_METRICS_ADDR", defaultMetricsAddr)
	epochURL := commonk8s.EnvOrDefault("EPOCH_URL", defaultEpochURL)
	epochToken := os.Getenv("EPOCH_TOKEN")
	leasesPath := commonk8s.EnvOrDefault("VK_LEASES_PATH", network.DefaultLeasesPath)
	cocoonBin := commonk8s.EnvOrDefault("VK_COCOON_BIN", "")
	sshPassword := os.Getenv("VK_SSH_PASSWORD")
	orphanPolicy := commonk8s.EnvOrDefault("VK_ORPHAN_POLICY", defaultOrphanPolicy)
	nodeIP := commonk8s.EnvOrDefault("VK_NODE_IP", "")
	nodePool := commonk8s.EnvOrDefault("VK_NODE_POOL", meta.DefaultNodePool)
	providerID := os.Getenv("VK_PROVIDER_ID")
	if nodeIP == "" {
		nodeIP = commonk8s.DetectNodeIP()
	}
	certPath := commonk8s.EnvOrDefault("VK_TLS_CERT", defaultTLSCert)
	keyPath := commonk8s.EnvOrDefault("VK_TLS_KEY", defaultTLSKey)

	cfg, err := commonk8s.LoadConfig()
	if err != nil {
		logger.Fatalf(ctx, err, "load kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf(ctx, err, "build clientset: %v", err)
	}

	metrics.Register(prometheus.DefaultRegisterer)

	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tlsCert, tlsSource, err := commonk8s.LoadOrGenerateCert(certPath, keyPath, nodeName, nodeIP)
	if err != nil {
		logger.Fatalf(signalCtx, err, "tls setup: %v", err)
	}
	logger.Infof(signalCtx, "kubelet TLS from %s", tlsSource)

	nodeCapacity, nodeAllocatable, err := provider.NodeResources()
	if err != nil {
		logger.Fatalf(signalCtx, err, "node resources: %v", err)
	}

	p := buildProvider(signalCtx, buildOpts{
		nodeName:     nodeName,
		epochURL:     epochURL,
		epochToken:   epochToken,
		leasesPath:   leasesPath,
		cocoonBin:    cocoonBin,
		sshPassword:  sshPassword,
		orphanPolicy: orphanPolicy,
		clientset:    clientset,
	})

	if reconcileErr := p.StartupReconcile(signalCtx); reconcileErr != nil {
		logger.Fatalf(signalCtx, reconcileErr, "startup reconcile failed; refusing to register node: %v", reconcileErr)
	}

	p.StartVMWatcher(signalCtx)

	newProvider := func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		if cfg.Node != nil {
			if cfg.Node.Labels == nil {
				cfg.Node.Labels = map[string]string{}
			}
			cfg.Node.Labels[meta.LabelNodePool] = nodePool
			cfg.Node.Labels["node-role.kubernetes.io/cocoon-vm"] = ""
			cfg.Node.Status.NodeInfo.KubeletVersion = version.VERSION
			cfg.Node.Status.Conditions = defaultNodeConditions()
			cfg.Node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP},
				{Type: corev1.NodeHostName, Address: nodeName},
			}
			cfg.Node.Status.Capacity = nodeCapacity
			cfg.Node.Status.Allocatable = nodeAllocatable
			cfg.Node.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletAPIPort},
			}
			if providerID != "" {
				cfg.Node.Spec.ProviderID = providerID
			}
			hasCocoonTaint := slices.ContainsFunc(cfg.Node.Spec.Taints, func(t corev1.Taint) bool {
				return t.Key == meta.TolerationKey
			})
			if !hasCocoonTaint {
				cfg.Node.Spec.Taints = append(cfg.Node.Spec.Taints, corev1.Taint{
					Key:    meta.TolerationKey,
					Value:  "cocoon",
					Effect: corev1.TaintEffectNoSchedule,
				})
			}
		}
		return p, cocoon.NewNodeProvider(), nil
	}

	kubeletMux := http.NewServeMux()
	n, err := nodeutil.NewNode(nodeName, newProvider,
		nodeutil.WithClient(clientset),
		nodeutil.AttachProviderRoutes(kubeletMux),
		withHandler(kubeletMux),
		nodeutil.WithTLSConfig(func(tc *tls.Config) error {
			tc.Certificates = []tls.Certificate{tlsCert}
			tc.ClientAuth = tls.NoClientCert
			return nil
		}),
	)
	if err != nil {
		logger.Fatalf(signalCtx, err, "create virtual-kubelet node: %v", err)
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Infof(signalCtx, "vk-cocoon metrics listening on %s", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(signalCtx, err, "metrics listen and serve")
		}
	}()

	go func() {
		logger.Infof(signalCtx, "vk-cocoon node %s kubelet API on :%d", nodeName, kubeletAPIPort)
		if err := n.Run(signalCtx); err != nil {
			logger.Fatalf(signalCtx, err, "virtual-kubelet node exited: %v", err)
		}
	}()

	// NaiveNodeProvider doesn't propagate DaemonEndpoints; patch directly.
	go patchKubeletEndpoint(signalCtx, clientset, nodeName)

	<-signalCtx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Warnf(shutdownCtx, "shutdown metrics: %v", err)
	}
	p.Close()
	logger.Info(shutdownCtx, "vk-cocoon exiting")
}

type buildOpts struct {
	nodeName     string
	epochURL     string
	epochToken   string
	leasesPath   string
	cocoonBin    string
	sshPassword  string
	orphanPolicy string
	clientset    kubernetes.Interface
}

func buildProvider(ctx context.Context, opts buildOpts) *cocoon.Provider {
	logger := log.WithFunc("buildProvider")
	registry := snapshots.New(opts.epochURL, opts.epochToken)
	runtime := vm.NewCocoonCLI(opts.cocoonBin, true)
	p := cocoon.NewProvider()
	p.NodeName = opts.nodeName
	p.Clientset = opts.clientset
	p.Runtime = runtime
	p.Puller = &snapshots.Puller{Registry: registry, Runtime: runtime}
	p.Pusher = &snapshots.Pusher{Registry: registry, Runtime: runtime}
	p.Registry = registry
	p.LeaseParser = network.NewLeaseParser(opts.leasesPath)
	// NopPinger fallback when CAP_NET_RAW is unavailable.
	if icmpPinger, err := network.NewICMPPinger(); err != nil {
		logger.Warnf(ctx, "icmp pinger disabled (%v); readiness will fall back to ip-resolved heuristic", err)
		p.Pinger = network.NopPinger{}
	} else {
		p.Pinger = icmpPinger
	}
	p.GuestSSH = guest.NewSSHExecutor(defaultSSHUser, opts.sshPassword, defaultSSHPort)
	p.GuestRDP = guest.RDPExecutor{}
	p.Probes = probes.NewManager(ctx)
	p.OrphanPolicy = provider.OrphanPolicy(strings.ToLower(opts.orphanPolicy))
	return p
}

func withHandler(h http.Handler) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.Handler = h
		return nil
	}
}

// patchKubeletEndpoint writes daemonEndpoints into the node status
// with retries to ride out the window between node creation and cache warm-up.
func patchKubeletEndpoint(ctx context.Context, clientset kubernetes.Interface, nodeName string) {
	logger := log.WithFunc("patchKubeletEndpoint")
	// Give v-k time to create the node object.
	if !commonk8s.SleepCtx(ctx, endpointPatchWait) {
		return
	}
	for attempt := range 10 {
		nodeObj, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			if !commonk8s.SleepCtx(ctx, endpointPatchRetry) {
				return
			}
			continue
		}
		nodeObj.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletAPIPort},
		}
		if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, nodeObj, metav1.UpdateOptions{}); err != nil {
			logger.Warnf(ctx, "patch daemon endpoints attempt %d: %v", attempt, err)
			if !commonk8s.SleepCtx(ctx, endpointPatchRetry) {
				return
			}
			continue
		}
		logger.Infof(ctx, "node %s kubelet endpoint set to :%d", nodeName, kubeletAPIPort)
		return
	}
}

func defaultNodeConditions() []corev1.NodeCondition {
	return []corev1.NodeCondition{
		{
			Type:    corev1.NodeReady,
			Status:  corev1.ConditionTrue,
			Reason:  "KubeletReady",
			Message: "vk-cocoon is ready",
		},
		{
			Type:    corev1.NodeDiskPressure,
			Status:  corev1.ConditionFalse,
			Reason:  "KubeletHasNoDiskPressure",
			Message: "vk-cocoon has no disk pressure",
		},
		{
			Type:    corev1.NodeMemoryPressure,
			Status:  corev1.ConditionFalse,
			Reason:  "KubeletHasSufficientMemory",
			Message: "vk-cocoon has sufficient memory",
		},
		{
			Type:    corev1.NodePIDPressure,
			Status:  corev1.ConditionFalse,
			Reason:  "KubeletHasSufficientPID",
			Message: "vk-cocoon has sufficient PID available",
		},
		{
			Type:    corev1.NodeNetworkUnavailable,
			Status:  corev1.ConditionFalse,
			Reason:  "RouteCreated",
			Message: "NodeController create implicit route",
		},
	}
}
