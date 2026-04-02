// Package provider — liveness/readiness probe runner for VMs.
//
// In containers, kubelet runs probes inside the container namespace.
// For VMs:
//   - exec probes: run command via SSH
//   - tcpSocket probes: TCP dial from host to VM IP:port
//   - httpGet probes: HTTP GET from host to VM
//
// Key difference from container probes: liveness failure does NOT kill
// the VM by default (VMs are stateful). Set cocoon.cis/liveness-restart=true
// to enable automatic VM restart (stop+start, preserves disk state).
package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

const (
	AnnLivenessRestart = "cocoon.cis/liveness-restart" // "true" to restart VM on liveness failure
)

// probeResult tracks the current probe state for a pod.
type probeResult struct {
	mu             sync.Mutex
	livenessReady  bool
	readinessReady bool
	liveFailCount  int
	readyFailCount int
	liveSuccCount  int
	readySuccCount int
	lastLiveErr    string
	lastReadyErr   string
	cancel         context.CancelFunc
}

// startProbes launches background probe goroutines for a pod.
// Call from CreatePod after the VM has an IP.
func (p *CocoonProvider) startProbes(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if len(pod.Spec.Containers) == 0 {
		return
	}
	c := pod.Spec.Containers[0]
	if c.LivenessProbe == nil && c.ReadinessProbe == nil {
		return
	}

	key := podKey(pod.Namespace, pod.Name)
	probeCtx, cancel := context.WithCancel(ctx)
	pr := &probeResult{
		livenessReady:  true, // assume healthy until proven otherwise
		readinessReady: true,
		cancel:         cancel,
	}

	p.mu.Lock()
	p.probeStates[key] = pr
	p.mu.Unlock()

	if c.LivenessProbe != nil {
		go p.runProbeLoop(probeCtx, pod.Namespace, pod.Name, vm, c.LivenessProbe, "liveness", pr)
	}
	if c.ReadinessProbe != nil {
		go p.runProbeLoop(probeCtx, pod.Namespace, pod.Name, vm, c.ReadinessProbe, "readiness", pr)
	}
	log.WithFunc("provider.startProbes").Infof(ctx, "%s: liveness=%v readiness=%v", key, c.LivenessProbe != nil, c.ReadinessProbe != nil)
}

// stopProbes cancels probe goroutines for a pod.
func (p *CocoonProvider) stopProbes(key string) {
	p.mu.Lock()
	if pr, ok := p.probeStates[key]; ok {
		pr.cancel()
		delete(p.probeStates, key)
	}
	p.mu.Unlock()
}

// runProbeLoop runs a single probe type (liveness or readiness) in a loop.
func (p *CocoonProvider) runProbeLoop(ctx context.Context, ns, name string, vm *CocoonVM, probe *corev1.Probe, probeType string, pr *probeResult) {
	delay := time.Duration(probe.InitialDelaySeconds) * time.Second
	period := time.Duration(probe.PeriodSeconds) * time.Second
	if period == 0 {
		period = 10 * time.Second
	}
	timeout := time.Duration(probe.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 1 * time.Second
	}
	failThresh := int(probe.FailureThreshold)
	if failThresh == 0 {
		failThresh = 3
	}
	succThresh := int(probe.SuccessThreshold)
	if succThresh == 0 {
		succThresh = 1
	}

	// Initial delay
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	logger := log.WithFunc("provider.runProbeLoop")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := p.executeProbe(ctx, vm, probe, timeout)
		pr.mu.Lock()
		if probeType == "liveness" { //nolint:nestif // probe state machine with threshold tracking
			if err == nil {
				pr.liveSuccCount++
				pr.liveFailCount = 0
				if pr.liveSuccCount >= succThresh {
					pr.livenessReady = true
				}
				pr.lastLiveErr = ""
			} else {
				pr.liveFailCount++
				pr.liveSuccCount = 0
				pr.lastLiveErr = err.Error()
				if pr.liveFailCount >= failThresh {
					if pr.livenessReady {
						logger.Warnf(ctx, "%s/%s liveness FAILED (%dx): %v", ns, name, pr.liveFailCount, err)
						pr.livenessReady = false
						go p.notifyPodStatus(ctx, ns, name)
					}
				}
			}
		} else { // readiness
			if err == nil {
				pr.readySuccCount++
				pr.readyFailCount = 0
				if pr.readySuccCount >= succThresh && !pr.readinessReady {
					pr.readinessReady = true
					go p.notifyPodStatus(ctx, ns, name)
				}
			} else {
				pr.readyFailCount++
				pr.readySuccCount = 0
				pr.lastReadyErr = err.Error()
				if pr.readyFailCount >= failThresh {
					if pr.readinessReady {
						logger.Warnf(ctx, "%s/%s readiness FAILED (%dx): %v", ns, name, pr.readyFailCount, err)
						pr.readinessReady = false
						go p.notifyPodStatus(ctx, ns, name)
					}
				}
			}
		}
		pr.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(period):
		}
	}
}

// executeProbe runs a single probe check.
func (p *CocoonProvider) executeProbe(ctx context.Context, vm *CocoonVM, probe *corev1.Probe, timeout time.Duration) error {
	if probe.TCPSocket != nil {
		return probeTCP(vm.ip, probe.TCPSocket.Port.IntValue(), timeout)
	}
	if probe.HTTPGet != nil {
		port := probe.HTTPGet.Port.IntValue()
		scheme := "http"
		if probe.HTTPGet.Scheme == corev1.URISchemeHTTPS {
			scheme = "https"
		}
		path := probe.HTTPGet.Path
		if path == "" {
			path = "/"
		}
		return probeHTTP(ctx, scheme, vm.ip, port, path, timeout)
	}
	if probe.Exec != nil && vm.os != osWindows {
		pw := p.sshPass(vm)
		cmd := strings.Join(probe.Exec.Command, " ")
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := sshExecSimple(probeCtx, vm, pw, cmd)
		return err
	}
	return nil // no probe handler defined
}

// probeTCP dials a TCP port.
func probeTCP(ip string, port int, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeout)
	if err != nil {
		return fmt.Errorf("tcp %s:%d: %w", ip, port, err)
	}
	_ = conn.Close()
	return nil
}

// probeClient is reused across all HTTP probes; timeout is set per-request
// via context rather than on the client.
var probeClient = &http.Client{}

// probeHTTP does an HTTP GET and checks for 2xx/3xx.
func probeHTTP(ctx context.Context, scheme, ip string, port int, path string, timeout time.Duration) error {
	url := fmt.Sprintf("%s://%s:%d%s", scheme, ip, port, path)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("http %s: %w", url, err)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return fmt.Errorf("http %s: %w", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// getProbeReadiness returns (livenessOK, readinessOK) for a pod.
func (p *CocoonProvider) getProbeReadiness(key string) (bool, bool) {
	p.mu.RLock()
	pr, ok := p.probeStates[key]
	p.mu.RUnlock()
	if !ok {
		return true, true // no probes = always ready
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.livenessReady, pr.readinessReady
}
