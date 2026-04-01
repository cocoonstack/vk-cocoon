package provider

import (
	"net/http"
	"testing"
)

func TestEpochPullerSetAuthFallsBackToEnvToken(t *testing.T) {
	t.Setenv("EPOCH_REGISTRY_TOKEN", "env-token")

	p := &EpochPuller{}
	req, err := http.NewRequest(http.MethodGet, "https://epoch.example.com/v2/demo/manifests/latest", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	p.setAuth(req)
	if got := req.Header.Get("Authorization"); got != "Bearer env-token" {
		t.Fatalf("authorization header mismatch: got %q", got)
	}
}

func TestEpochPullerEnvTokenOverridesCachedToken(t *testing.T) {
	t.Setenv("EPOCH_REGISTRY_TOKEN", "env-token")

	p := &EpochPuller{token: "stale-token"}
	if got := p.authToken(); got != "env-token" {
		t.Fatalf("auth token mismatch: got %q", got)
	}
}
