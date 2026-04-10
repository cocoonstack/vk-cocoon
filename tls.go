package main

import (
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
	"os"
	"time"
)

// loadOrGenerateTLS tries to load a TLS keypair from disk first; if
// either file is missing it falls back to an in-memory self-signed
// certificate valid for hostname / ip. The self-signed path is
// meant for dev and bring-up — production installs should mount
// real certs at the supplied paths (see VK_TLS_CERT / VK_TLS_KEY in
// the systemd unit).
func loadOrGenerateTLS(certPath, keyPath, hostname, ip string) (tls.Certificate, string, error) {
	if certPath != "" && keyPath != "" {
		if _, err := os.Stat(certPath); err == nil {
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return tls.Certificate{}, "", fmt.Errorf("load TLS keypair %s: %w", certPath, err)
			}
			return cert, fmt.Sprintf("disk %s", certPath), nil
		}
	}
	cert, err := generateSelfSignedCert(hostname, ip)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate self-signed cert: %w", err)
	}
	return cert, "self-signed", nil
}

// generateSelfSignedCert creates an in-memory ECDSA P-256 keypair
// and returns it as a tls.Certificate. The certificate is valid
// for ten years (long enough for dev) against hostname and ip.
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

// detectNodeIP walks the host's network interfaces and returns the
// first non-loopback IPv4 address. Used as the default VK_NODE_IP
// when the operator does not specify one explicitly.
func detectNodeIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return "127.0.0.1"
}
