package provider

import (
	"context"
	"errors"
	"testing"
)

func TestApplyEpochCreateSourceFallsBackWhenSnapshotPullFails(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		pulled: make(map[string]bool),
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "linux-image" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return errors.New("boom")
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeRun,
		image:      "linux-image",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeRun,
		effectiveMode: modeRun,
		runImage:      "https://epoch.example.com/linux-image",
		cloneImage:    "base-snapshot",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeRun {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeRun)
	}
	if plan.cloneImage != "base-snapshot" {
		t.Fatalf("cloneImage = %q, want base-snapshot", plan.cloneImage)
	}
}

func TestApplyEpochCreateSourceUsesSnapshotWhenLinuxRunPullSucceeds(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		pulled: make(map[string]bool),
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "linux-image" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return nil
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeRun,
		image:      "linux-image",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeRun,
		effectiveMode: modeRun,
		runImage:      "https://epoch.example.com/linux-image",
		cloneImage:    "base-snapshot",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeClone {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeClone)
	}
	if plan.cloneImage != "linux-image" {
		t.Fatalf("cloneImage = %q, want linux-image", plan.cloneImage)
	}
}

// OCI manifests short-circuit: EnsureSnapshotTag returns errOCIManifest,
// applyEpochCreateSource keeps modeRun with the original image ref so the
// cocoon CLI pulls via go-containerregistry.
func TestApplyEpochCreateSourceKeepsRunModeForOCIImage(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		pulled: make(map[string]bool),
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "ubuntu-24-04-bytedance" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return errOCIManifest
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeRun,
		image:      "ubuntu-24-04-bytedance",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeRun,
		effectiveMode: modeRun,
		runImage:      "https://epoch.example.com/ubuntu-24-04-bytedance",
		cloneImage:    "",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeRun {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeRun)
	}
	if plan.cloneImage != "" {
		t.Fatalf("cloneImage = %q, want empty", plan.cloneImage)
	}
}

// Clone mode with an OCI target degrades to cold boot with the image ref.
func TestApplyEpochCreateSourceCloneDegradesToRunForOCIImage(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		pulled: make(map[string]bool),
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "ubuntu-24-04-bytedance" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return errOCIManifest
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeClone,
		image:      "ubuntu-24-04-bytedance",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeClone,
		effectiveMode: modeClone,
		cloneImage:    "ubuntu-24-04-bytedance",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeRun {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeRun)
	}
	if plan.runImage != "ubuntu-24-04-bytedance" {
		t.Fatalf("runImage = %q, want ubuntu-24-04-bytedance", plan.runImage)
	}
}

// IsOCIImage detects OCI and Docker manifest media types.
func TestEpochManifestDocumentIsOCIImage(t *testing.T) {
	cases := []struct {
		mt   string
		want bool
	}{
		{"application/vnd.oci.image.manifest.v1+json", true},
		{"application/vnd.oci.image.index.v1+json", true},
		{"application/vnd.docker.distribution.manifest.v2+json", true},
		{"application/vnd.docker.distribution.manifest.list.v2+json", true},
		{"application/vnd.cocoon.snapshot.v1+json", false},
		{"application/vnd.epoch.manifest.v1+json", false},
		{"", false},
	}
	for _, tc := range cases {
		doc := &epochManifestDocument{MediaType: tc.mt}
		if got := doc.IsOCIImage(); got != tc.want {
			t.Errorf("IsOCIImage(%q) = %v, want %v", tc.mt, got, tc.want)
		}
	}
}
