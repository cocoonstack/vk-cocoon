// HTTP-based Epoch client for vk-cocoon.
//
// Handles both pull (download) and push (upload) of snapshots via the Epoch
// registry server's V2 API. This removes any direct object-store dependency
// from the provider.
package provider

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cocoonstack/epoch/cocoon"
)

// EpochPuller pulls snapshots from the Epoch HTTP registry.
type EpochPuller struct {
	serverURL string
	rootDir   string
	cocoonBin string
	token     string // Bearer token for registry auth (empty = no auth)
	client    *http.Client
	paths     *cocoon.Paths

	mu          sync.Mutex
	pulled      map[string]bool
	oci         map[string]bool        // refs whose manifest is an OCI image
	ensureLocks map[string]*sync.Mutex // per-ref mutex, serializes concurrent Ensure* calls

	ensureSnapshotTagFn   func(context.Context, string, string) error
	ensureCloudImageTagFn func(context.Context, string, string) error
}

// NewEpochPuller creates a puller that talks to the Epoch HTTP server.
// If EPOCH_REGISTRY_TOKEN is set, it is sent as a Bearer token on every request.
func NewEpochPuller(serverURL, rootDir, cocoonBin string) *EpochPuller {
	cocoonBin = cmp.Or(strings.TrimSpace(cocoonBin), "cocoon")
	return &EpochPuller{
		serverURL: serverURL,
		rootDir:   rootDir,
		cocoonBin: cocoonBin,
		token:     os.Getenv("EPOCH_REGISTRY_TOKEN"),
		client: &http.Client{
			// No global timeout — large blob downloads can take minutes.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		paths:  cocoon.NewPaths(rootDir),
		pulled: make(map[string]bool),
	}
}

// setAuth adds Bearer token to an HTTP request if configured.
func (p *EpochPuller) setAuth(req *http.Request) {
	if token := p.authToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (p *EpochPuller) authToken() string {
	if token := strings.TrimSpace(os.Getenv("EPOCH_REGISTRY_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(p.token)
}

// markPulled records a ref as pulled, clearing the cache when it grows too large.
func (p *EpochPuller) markPulled(ref string) {
	p.mu.Lock()
	if len(p.pulled) > 1000 {
		clear(p.pulled)
	}
	p.pulled[ref] = true
	p.mu.Unlock()
}

func (p *EpochPuller) cachedPull(ref string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pulled[ref]
}

// markOCI records that a ref resolved to an OCI manifest so subsequent
// Ensure* calls can short-circuit without re-fetching the manifest.
func (p *EpochPuller) markOCI(ref string) {
	p.mu.Lock()
	if p.oci == nil {
		p.oci = make(map[string]bool)
	}
	p.oci[ref] = true
	p.mu.Unlock()
}

func (p *EpochPuller) cachedOCI(ref string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.oci[ref]
}

// ensureLock returns a per-ref mutex that serializes concurrent Ensure*
// calls for the same image reference. Two concurrent EnsureSnapshotTag calls
// for the same ref would otherwise race on the local cocoon snapshot import
// (and, in the cloudimg path, on the local image store). The mutex map grows
// unbounded until EpochPuller is released — acceptable because refs are
// scoped per-server and bounded by the number of images a provider touches.
func (p *EpochPuller) ensureLock(ref string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ensureLocks == nil {
		p.ensureLocks = make(map[string]*sync.Mutex)
	}
	lock := p.ensureLocks[ref]
	if lock == nil {
		lock = &sync.Mutex{}
		p.ensureLocks[ref] = lock
	}
	return lock
}

// RootDir returns the cocoon data root directory.
func (p *EpochPuller) RootDir() string { return p.rootDir }

func (p *EpochPuller) manifestURL(name, tag string) string {
	return fmt.Sprintf("%s/v2/%s/manifests/%s", p.serverURL, name, tag)
}

func (p *EpochPuller) blobURL(name, digest string) string {
	return fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", p.serverURL, name, digest)
}

func (p *EpochPuller) tagDeleteURL(name, tag string) string {
	return fmt.Sprintf("%s/api/repositories/%s/tags/%s", p.serverURL, name, tag)
}
