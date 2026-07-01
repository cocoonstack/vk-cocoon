package main

import (
	"testing"

	"github.com/cocoonstack/epoch/registryclient"
	"github.com/cocoonstack/vk-cocoon/snapshots"
)

func TestBuildRegistryBackend(t *testing.T) {
	t.Setenv("EPOCH_CA_CERT", "") // keep the epoch client deterministic on dev machines

	oci, err := buildRegistry(buildOpts{ociRegistry: "example.com/proj/repo"})
	if err != nil {
		t.Fatalf("buildRegistry(OCI): %v", err)
	}
	if _, ok := oci.(*snapshots.OCIRegistry); !ok {
		t.Fatalf("OCI_REGISTRY set: got %T, want *snapshots.OCIRegistry", oci)
	}

	ep, err := buildRegistry(buildOpts{epochURL: "http://epoch.example"})
	if err != nil {
		t.Fatalf("buildRegistry(epoch): %v", err)
	}
	if _, ok := ep.(*registryclient.Client); !ok {
		t.Fatalf("no OCI_REGISTRY: got %T, want *registryclient.Client", ep)
	}
}
