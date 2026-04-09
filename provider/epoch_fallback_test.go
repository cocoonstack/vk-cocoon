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

// TestApplyEpochCreateSourceKeepsRunModeForOCIImage asserts that when
// EnsureSnapshotTag returns the OCI sentinel, the create plan stays in
// modeRun with the original image ref so cocoon's CH backend can pull it.
func TestApplyEpochCreateSourceKeepsRunModeForOCIImage(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "ubuntu-oci" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return errOCIManifest
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeRun,
		image:      "ubuntu-oci",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeRun,
		effectiveMode: modeRun,
		runImage:      "https://epoch.example.com/ubuntu-oci",
		cloneImage:    "",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeRun {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeRun)
	}
	if plan.cloneImage != "" {
		t.Fatalf("cloneImage = %q, want empty (run mode)", plan.cloneImage)
	}
}

// TestApplyEpochCreateSourceCloneDegradesToRunForOCIImage asserts that a
// clone target which turns out to be an OCI image degrades to a cold run
// using the would-have-been-cloned image as the runImage.
func TestApplyEpochCreateSourceCloneDegradesToRunForOCIImage(t *testing.T) {
	registryURL := "https://epoch.example.com"
	p := newTestProvider()
	p.pullers[registryURL] = &EpochPuller{
		ensureSnapshotTagFn: func(_ context.Context, name, tag string) error {
			if name != "ubuntu-oci" || tag != "latest" {
				t.Fatalf("EnsureSnapshotTag got %s:%s", name, tag)
			}
			return errOCIManifest
		},
	}

	req := createRequest{
		key:        "testns/app",
		mode:       modeClone,
		image:      "ubuntu-oci",
		osType:     "linux",
		loggerFunc: "test.applyEpochCreateSource",
	}
	plan := createPlan{
		registryURL:   registryURL,
		requestedMode: modeClone,
		effectiveMode: modeClone,
		cloneImage:    "ubuntu-oci",
	}

	p.applyEpochCreateSource(context.Background(), req, &plan)

	if plan.effectiveMode != modeRun {
		t.Fatalf("effectiveMode = %q, want %q", plan.effectiveMode, modeRun)
	}
	if plan.runImage != "ubuntu-oci" {
		t.Fatalf("runImage = %q, want ubuntu-oci", plan.runImage)
	}
}
