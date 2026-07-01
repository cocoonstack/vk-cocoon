package main

import (
	"testing"

	"github.com/cocoonstack/cocoon-common/oci"
)

func TestBuildRegistry(t *testing.T) {
	reg, err := buildRegistry(buildOpts{ociRegistry: "example.com/proj/repo"})
	if err != nil {
		t.Fatalf("buildRegistry: %v", err)
	}
	if _, ok := reg.(*oci.OCIRegistry); !ok {
		t.Fatalf("got %T, want *oci.OCIRegistry", reg)
	}

	if _, err := buildRegistry(buildOpts{}); err == nil {
		t.Fatal("buildRegistry with no OCI_REGISTRY: want error, got nil")
	}
}
