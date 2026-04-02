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

	mu     sync.Mutex
	pulled map[string]bool

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
