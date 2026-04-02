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
