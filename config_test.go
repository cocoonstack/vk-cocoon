package main

import (
	"errors"
	"testing"
)

func TestExplicitKubeconfigPathFromEnvPrefersEnvironment(t *testing.T) {
	got := explicitKubeconfigPathFromEnv("/tmp/custom-kubeconfig", func(string) error {
		t.Fatal("stat should not be called when KUBECONFIG is set")
		return nil
	})
	if got != "/tmp/custom-kubeconfig" {
		t.Fatalf("explicitKubeconfigPathFromEnv = %q, want /tmp/custom-kubeconfig", got)
	}
}

func TestExplicitKubeconfigPathFromEnvUsesDefaultPathWhenPresent(t *testing.T) {
	got := explicitKubeconfigPathFromEnv("", func(path string) error {
		if path != defaultKubeconfigPath {
			t.Fatalf("stat path = %q, want %q", path, defaultKubeconfigPath)
		}
		return nil
	})
	if got != defaultKubeconfigPath {
		t.Fatalf("explicitKubeconfigPathFromEnv = %q, want %q", got, defaultKubeconfigPath)
	}
}

func TestExplicitKubeconfigPathFromEnvFallsBackWhenDefaultMissing(t *testing.T) {
	got := explicitKubeconfigPathFromEnv("", func(string) error {
		return errors.New("missing")
	})
	if got != "" {
		t.Fatalf("explicitKubeconfigPathFromEnv = %q, want empty", got)
	}
}
