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

	"golang.org/x/sync/singleflight"

	"github.com/cocoonstack/epoch/cocoon"
)

// refStateCacheCap caps the number of entries kept in the per-ref state map
// before it is cleared wholesale. The cap exists to bound memory on a
// long-lived provider that touches many distinct image names; the cleared
// state is the safe default (re-resolution on next request).
const refStateCacheCap = 1000

// refState records what we know about an image reference between calls.
type refState int

const (
	refStateUnknown  refState = iota
	refStateImported          // pulled and imported as a cocoon-native snapshot or cloud image
	refStateOCI               // resolved to an OCI / Docker manifest; cocoon pulls directly
)

// EpochPuller pulls snapshots from the Epoch HTTP registry.
type EpochPuller struct {
	serverURL string
	rootDir   string
	cocoonBin string
	token     string // Bearer token for registry auth (empty = no auth)
	client    *http.Client
	paths     *cocoon.Paths

	// sf serializes concurrent Ensure* calls for the same ref so two
	// callers do not race on the local cocoon import. The key namespace
	// (`snapshot:` / `cloudimg:`) is set by the caller. singleflight evicts
	// keys automatically when the in-flight call returns, so unlike a
	// per-ref mutex map there is no leak.
	sf singleflight.Group

	mu     sync.Mutex // mu guards states.
	states map[string]refState

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
		paths: cocoon.NewPaths(rootDir),
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

// cachedState returns the cached state for a ref. refStateUnknown means
// "no information" — the caller should resolve normally. Note that
// refStateOCI entries are dropped along with everything else when the
// cache hits its cap, which costs an extra manifest fetch on the next
// resolution but never produces an incorrect result.
func (p *EpochPuller) cachedState(ref string) refState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[ref]
}

// markRef sets the cached state for a ref, capping total cache size by
// clearing wholesale when it reaches refStateCacheCap. The clear() runs
// before the new entry is inserted, so the new entry always survives.
func (p *EpochPuller) markRef(ref string, state refState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states == nil {
		p.states = make(map[string]refState)
	}
	if len(p.states) >= refStateCacheCap {
		clear(p.states)
	}
	p.states[ref] = state
}

// doDeduped runs fn under singleflight, deduplicating concurrent callers
// that share the same key. Used by EnsureSnapshotTag and EnsureCloudImageTag
// so two callers cannot race on the local cocoon import.
func (p *EpochPuller) doDeduped(key string, fn func() error) error {
	_, err, _ := p.sf.Do(key, func() (any, error) {
		return nil, fn()
	})
	return err
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
