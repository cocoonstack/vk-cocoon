package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/cocoonstack/cocoon-operator/pkg/k8sutil"

	"github.com/cocoonstack/vk-cocoon/provider"
)

func main() {
	nodeName := os.Getenv("VK_NODE_NAME")
	if nodeName == "" {
		nodeName = "cocoon-pool"
	}
	cocoonBin := os.Getenv("COCOON_BIN")
	if cocoonBin == "" {
		cocoonBin = "/usr/local/bin/cocoon"
	}
	nodeIP := os.Getenv("VK_NODE_IP")
	if nodeIP == "" {
		nodeIP = detectNodeIP()
	}

	config, err := k8sutil.LoadConfig()
	if err != nil {
		klog.Fatalf("kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("clientset: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mux := http.NewServeMux()

	newProvider := func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		if cfg.Node != nil {
			cfg.Node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP},
				{Type: corev1.NodeHostName, Address: nodeName},
			}
			cfg.Node.Status.Capacity = provider.NodeCapacity()
			cfg.Node.Status.Allocatable = cfg.Node.Status.Capacity
			cfg.Node.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250},
			}
		}
		np := provider.NewCocoonNodeProvider(cfg.Node)
		return provider.NewCocoonProvider(ctx, cocoonBin, nodeIP, clientset, cfg), np, nil
	}

	// Load TLS cert (signed by k3s server-ca, or self-signed)
	certPath := os.Getenv("VK_TLS_CERT")
	keyPath := os.Getenv("VK_TLS_KEY")
	if certPath == "" {
		certPath = "/home/tiger/bin/tls/vk-kubelet.crt"
	}
	if keyPath == "" {
		keyPath = "/home/tiger/bin/tls/vk-kubelet.key"
	}

	var tlsCert tls.Certificate
	if _, statErr := os.Stat(certPath); statErr == nil {
		var loadErr error
		tlsCert, loadErr = tls.LoadX509KeyPair(certPath, keyPath)
		if loadErr != nil {
			klog.Fatalf("load TLS cert: %v", loadErr)
		}
		klog.Infof("Using TLS cert from %s", certPath)
	} else {
		var genErr error
		tlsCert, genErr = generateSelfSignedCert(nodeName, nodeIP)
		if genErr != nil {
			klog.Fatalf("generate TLS cert: %v", genErr)
		}
		klog.Infof("Using self-signed TLS cert")
	}

	n, err := nodeutil.NewNode(nodeName, newProvider,
		nodeutil.WithClient(clientset),
		nodeutil.AttachProviderRoutes(mux),
		withHandler(mux),
		nodeutil.WithTLSConfig(func(cfg *tls.Config) error {
			cfg.Certificates = []tls.Certificate{tlsCert}
			cfg.ClientAuth = tls.NoClientCert
			return nil
		}),
	)
	if err != nil {
		klog.Fatalf("failed to create node: %v", err)
	}

	go func() {
		if err := n.Run(ctx); err != nil {
			klog.Fatalf("node exited: %v", err)
		}
	}()

	// Patch node to set kubelet endpoint port (NaiveNodeProvider doesn't propagate DaemonEndpoints)
	go func() {
		time.Sleep(5 * time.Second)
		for i := range 10 {
			nodeObj, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}
			nodeObj.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250},
			}
			if _, err := clientset.CoreV1().Nodes().UpdateStatus(ctx, nodeObj, metav1.UpdateOptions{}); err != nil {
				klog.Warningf("patch node endpoints: %v (retry %d)", err, i)
				time.Sleep(2 * time.Second)
				continue
			}
			klog.Infof("Node %s kubelet endpoint set to :10250", nodeName)
			break
		}
	}()

	klog.Infof("Virtual Kubelet '%s' started (ip=%s, cocoon=%s)", nodeName, nodeIP, cocoonBin)
	fmt.Printf("Virtual Kubelet '%s' ready — kubelet API on :10250\n", nodeName)
	<-ctx.Done()
	fmt.Println("Shutting down...")
}

func withHandler(h http.Handler) nodeutil.NodeOpt {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.Handler = h
		return nil
	}
}

// generateSelfSignedCert creates an in-memory TLS certificate for the kubelet API.
func generateSelfSignedCert(hostname, ip string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP(ip), net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

func detectNodeIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
				continue
			}
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return "127.0.0.1"
}
