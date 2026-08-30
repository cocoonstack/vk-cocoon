// Package main is the vk-cocoon entry point. vk-cocoon is the virtual-kubelet
// provider that maps Kubernetes pods to cocoon MicroVMs.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/projecteru2/core/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	commonhttpx "github.com/cocoonstack/cocoon-common/httpx"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	commonlog "github.com/cocoonstack/cocoon-common/log"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"

	"github.com/cocoonstack/vk-cocoon/guest/sac"
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
	defaultOrphanPolicy = string(provider.OrphanDestroy)
	defaultRestoreMode  = string(vm.RestoreOnDemand)
	// defaultStagingDir must share a filesystem with cocoon's data dir (clone hardlinks).
	defaultStagingDir = "/var/lib/cocoon/vk-staging"
	// defaultPeerAddr serves snapshots to waking peers; they dial this port by convention.
	defaultPeerAddr = ":12501"
	// defaultCocoonSnapshotDir is cocoon's localfile store root the peer server reads.
	defaultCocoonSnapshotDir = "/var/lib/cocoon/snapshot/localfile"

	defaultTLSCert        = "/etc/cocoon/vk/tls/vk-kubelet.crt"
	defaultTLSKey         = "/etc/cocoon/vk/tls/vk-kubelet.key"
	defaultKubeletAPIPort = 10250
	endpointPatchWait     = 5 * time.Second
	endpointPatchRetry    = 2 * time.Second
	shutdownTimeout       = 10 * time.Second
)

func main() {
	ctx := context.Background()
	if err := commonlog.Setup(ctx, "VK_LOG_LEVEL"); err != nil {
		fmt.Fprintf(os.Stderr, "log setup: %v\n", err)
		os.Exit(1)
	}

	logger := log.WithFunc("main")
	logger.Infof(ctx, "vk-cocoon %s starting (rev=%s built=%s)",
		version.VERSION, version.REVISION, version.BUILTAT)

	nodeName := commonk8s.EnvOrDefault("VK_NODE_NAME", defaultNodeName)
	metricsAddr := commonk8s.EnvOrDefault("VK_METRICS_ADDR", defaultMetricsAddr)
	ociRegistry := os.Getenv("OCI_REGISTRY")
	leasesPath := commonk8s.EnvOrDefault("VK_LEASES_PATH", network.DefaultLeasesPath)
	controlSocket := commonk8s.EnvOrDefault("VK_COCOON_NET_CONTROL_SOCKET", network.DefaultControlSocket)
	cocoonBin := commonk8s.EnvOrDefault("VK_COCOON_BIN", "")
	macosBin := commonk8s.EnvOrDefault("VK_COCOON_MACOS_BIN", "")
	macosVNCPassword := os.Getenv("COCOON_MACOS_VNC_PASSWORD")
	orphanPolicy := commonk8s.EnvOrDefault("VK_ORPHAN_POLICY", defaultOrphanPolicy)
	restoreMode := commonk8s.EnvOrDefault("VK_RESTORE_MODE", defaultRestoreMode)
	stagingDir := commonk8s.EnvOrDefault("VK_STAGING_DIR", defaultStagingDir)
	peerAddr := commonk8s.EnvOrDefault("VK_PEER_ADDR", defaultPeerAddr)
	cocoonSnapshotDir := commonk8s.EnvOrDefault("VK_COCOON_SNAPSHOT_DIR", defaultCocoonSnapshotDir)
	_, peerPort, portErr := net.SplitHostPort(peerAddr)
	if portErr != nil {
		logger.Fatalf(ctx, portErr, "parse VK_PEER_ADDR %q", peerAddr)
	}
	nodeIP := commonk8s.EnvOrDefault("VK_NODE_IP", "")
	nodePool := commonk8s.EnvOrDefault("VK_NODE_POOL", meta.DefaultNodePool)
	snapshotCompatibilityClass := os.Getenv("VK_SNAPSHOT_CPU_CLASS")
	if errs := utilvalidation.IsValidLabelValue(snapshotCompatibilityClass); len(errs) != 0 {
		logger.Fatalf(ctx, errors.New(strings.Join(errs, "; ")), "invalid VK_SNAPSHOT_CPU_CLASS %q", snapshotCompatibilityClass)
	}
	providerID := os.Getenv("VK_PROVIDER_ID")
	if nodeIP == "" {
		detected, err := commonk8s.DetectNodeIP()
		if err != nil {
			logger.Fatalf(ctx, err, "detect node ip")
		}
		nodeIP = detected
	}
	certPath := commonk8s.EnvOrDefault("VK_TLS_CERT", defaultTLSCert)
	keyPath := commonk8s.EnvOrDefault("VK_TLS_KEY", defaultTLSKey)

	clientset, err := commonk8s.NewClientset()
	if err != nil {
		logger.Fatalf(ctx, err, "build clientset")
	}

	metrics.Register(prometheus.DefaultRegisterer)

	signalCtx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tlsCert, tlsSource, err := commonk8s.LoadOrGenerateCert(signalCtx, certPath, keyPath, nodeName, nodeIP)
	if err != nil {
		logger.Fatalf(signalCtx, err, "tls setup")
	}
	logger.Infof(signalCtx, "kubelet TLS from %s", tlsSource)

	nodeCapacity, nodeAllocatable, err := provider.NodeResources()
	if err != nil {
		logger.Fatalf(signalCtx, err, "node resources")
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	defer broadcaster.Shutdown()
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "vk-cocoon", Host: nodeName})

	go snapshots.SweepStaging(signalCtx, stagingDir)

	p, err := buildProvider(signalCtx, buildOpts{
		nodeName:                   nodeName,
		snapshotCompatibilityClass: snapshotCompatibilityClass,
		ociRegistry:                ociRegistry,
		leasesPath:                 leasesPath,
		controlSocket:              controlSocket,
		cocoonBin:                  cocoonBin,
		macosBin:                   macosBin,
		macosVNCPassword:           macosVNCPassword,
		orphanPolicy:               orphanPolicy,
		restoreMode:                restoreMode,
		stagingDir:                 stagingDir,
		peerPort:                   peerPort,
		clientset:                  clientset,
		recorder:                   recorder,
	})
	if err != nil {
		logger.Fatalf(signalCtx, err, "build provider")
	}

	if reconcileErr := p.StartupReconcile(signalCtx); reconcileErr != nil {
		logger.Fatalf(signalCtx, reconcileErr, "startup reconcile failed; refusing to register node")
	}

	p.StartVMWatcher(signalCtx)
	p.StartLifecycleReconciler()

	newProvider := func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		if cfg.Node != nil {
			applyNodeLabels(cfg.Node, nodePool, snapshotCompatibilityClass)
			cfg.Node.Status.NodeInfo.KubeletVersion = version.VERSION
			cfg.Node.Status.Conditions = defaultNodeConditions()
			cfg.Node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP},
				{Type: corev1.NodeHostName, Address: nodeName},
			}
			cfg.Node.Status.Capacity = nodeCapacity
			cfg.Node.Status.Allocatable = nodeAllocatable
			cfg.Node.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletAPIPort()},
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
	n, err := nodeutil.NewNode(
		nodeName, newProvider,
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
		logger.Fatalf(signalCtx, err, "create virtual-kubelet node")
	}

	prometheus.DefaultRegisterer.MustRegister(metrics.NewVMCollector(p.CollectVMStats))

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
	metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	metricsServer := commonhttpx.NewServer(metricsAddr, metricsMux)

	peerServer := commonhttpx.NewServer(peerAddr, (&snapshots.PeerServer{
		Snapshots: p.Runtime,
		StoreDir:  cocoonSnapshotDir,
	}).Handler())

	go func() {
		logger.Infof(signalCtx, "vk-cocoon node %s kubelet API on :%d", nodeName, kubeletAPIPort())
		if err := n.Run(signalCtx); err != nil {
			logger.Fatalf(signalCtx, err, "virtual-kubelet node exited")
		}
	}()

	// NaiveNodeProvider doesn't propagate DaemonEndpoints; patch directly.
	go patchNodeLabelsAndEndpoint(signalCtx, clientset, nodeName, nodePool, snapshotCompatibilityClass)

	logger.Infof(signalCtx, "vk-cocoon metrics listening on %s, peer snapshots on %s", metricsAddr, peerAddr)
	if err := commonhttpx.Run(signalCtx, shutdownTimeout,
		commonhttpx.HTTPServerSpec(metricsServer),
		commonhttpx.HTTPServerSpec(peerServer),
	); err != nil {
		logger.Error(signalCtx, err, "http servers exited with error")
	}
	p.Close()
	logger.Info(signalCtx, "vk-cocoon exiting")
}

type buildOpts struct {
	nodeName                   string
	snapshotCompatibilityClass string
	ociRegistry                string
	leasesPath                 string
	controlSocket              string
	cocoonBin                  string
	macosBin                   string
	macosVNCPassword           string
	orphanPolicy               string
	restoreMode                string
	stagingDir                 string
	peerPort                   string
	clientset                  kubernetes.Interface
	recorder                   record.EventRecorder
}

func buildRegistry(opts buildOpts) (oci.Registry, error) {
	if opts.ociRegistry == "" {
		return nil, errors.New("OCI_REGISTRY must be set")
	}
	keychain := authn.NewMultiKeychain(google.Keychain, authn.DefaultKeychain)
	return oci.NewOCIRegistry(opts.ociRegistry, keychain), nil
}

func buildProvider(ctx context.Context, opts buildOpts) (*cocoon.Provider, error) {
	logger := log.WithFunc("buildProvider")
	restoreMode, err := vm.ParseRestoreMode(opts.restoreMode)
	if err != nil {
		return nil, fmt.Errorf("parse VK_RESTORE_MODE: %w", err)
	}
	registry, err := buildRegistry(opts)
	if err != nil {
		return nil, fmt.Errorf("construct registry client: %w", err)
	}
	logger.Infof(ctx, "registry backend: OCI %s", opts.ociRegistry)
	runtime := vm.NewCocoonCLI(opts.cocoonBin)
	p := cocoon.NewProvider(ctx)
	p.NodeName = opts.nodeName
	p.SnapshotCompatibilityClass = opts.snapshotCompatibilityClass
	p.Clientset = opts.clientset
	p.Recorder = opts.recorder
	p.Runtime = runtime
	p.MacosBin = opts.macosBin
	p.MacosVNCPassword = opts.macosVNCPassword
	transfer := snapshots.TransferConfigFromEnv()
	p.Puller = &snapshots.Puller{Registry: registry, Runtime: runtime, Transfer: transfer}
	p.Pusher = &snapshots.Pusher{Registry: registry, Runtime: runtime, Transfer: transfer, NodeName: opts.nodeName}
	p.PeerRestorer = &snapshots.PeerRestorer{StagingRoot: opts.stagingDir}
	p.PeerPort = opts.peerPort
	p.Registry = registry
	p.LeaseParser = network.NewLeaseParser(opts.leasesPath)
	p.LeaseReleaser = network.NewCocoonNetLeaseReleaser(opts.controlSocket)
	if icmpPinger, err := network.NewICMPPinger(); err != nil {
		logger.Warnf(ctx, "icmp pinger disabled (%v); readiness will fall back to ip-resolved heuristic", err)
		p.Pinger = network.NopPinger{}
	} else {
		p.Pinger = icmpPinger
	}
	p.GuestSAC = &sac.Dialer{}
	p.Probes = probes.NewManager(ctx)
	p.OrphanPolicy = provider.OrphanPolicy(strings.ToLower(opts.orphanPolicy))
	p.RestoreMode = restoreMode
	return p, nil
}

func applyNodeLabels(node *corev1.Node, nodePool, snapshotCompatibilityClass string) {
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[meta.LabelNodePool] = nodePool
	node.Labels["node-role.kubernetes.io/cocoon-vm"] = ""
	if snapshotCompatibilityClass == "" {
		delete(node.Labels, meta.LabelSnapshotCompatibilityClass)
		return
	}
	node.Labels[meta.LabelSnapshotCompatibilityClass] = snapshotCompatibilityClass
}

// kubeletAPIPort is overridable via VK_KUBELET_PORT so a co-located kubelet (e.g. k3s) can keep :10250.
func kubeletAPIPort() int32 {
	if p, err := strconv.ParseUint(os.Getenv("VK_KUBELET_PORT"), 10, 16); err == nil && p > 0 {
		return int32(p)
	}
	return defaultKubeletAPIPort
}

func withHandler(h http.Handler) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.Handler = h
		cfg.HTTPListenAddr = fmt.Sprintf(":%d", kubeletAPIPort())
		return nil
	}
}

// patchNodeLabelsAndEndpoint re-asserts node labels and daemonEndpoints with
// retries to ride out the node-creation window; v-k only patches status after
// creation, so a re-stamped snapshot CPU class never lands on its own.
func patchNodeLabelsAndEndpoint(ctx context.Context, clientset kubernetes.Interface, nodeName, nodePool, snapshotCompatibilityClass string) {
	logger := log.WithFunc("patchNodeLabelsAndEndpoint")
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
		applyNodeLabels(nodeObj, nodePool, snapshotCompatibilityClass)
		updated, err := clientset.CoreV1().Nodes().Update(ctx, nodeObj, metav1.UpdateOptions{})
		if err != nil {
			logger.Errorf(ctx, err, "re-assert node labels attempt %d", attempt)
			if !commonk8s.SleepCtx(ctx, endpointPatchRetry) {
				return
			}
			continue
		}
		updated.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletAPIPort()},
		}
		if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			logger.Errorf(ctx, err, "patch daemon endpoints attempt %d", attempt)
			if !commonk8s.SleepCtx(ctx, endpointPatchRetry) {
				return
			}
			continue
		}
		logger.Infof(ctx, "node %s kubelet endpoint set to :%d", nodeName, kubeletAPIPort())
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
