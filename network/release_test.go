package network

import (
	"net"
	"net/http"
	"os"
	"testing"
)

func TestCocoonNetLeaseReleaser(t *testing.T) {
	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	got := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	releaser := NewCocoonNetLeaseReleaser(socketPath)
	if err := releaser.ReleaseByMAC(t.Context(), "AA-BB-CC-DD-EE-FF"); err != nil {
		t.Fatalf("ReleaseByMAC: %v", err)
	}
	if request := <-got; request != "DELETE /v1/leases/aa:bb:cc:dd:ee:ff" {
		t.Errorf("request = %q", request)
	}
}

func TestCocoonNetLeaseReleaserRetriesA500Once(t *testing.T) {
	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	hits := make(chan struct{}, 4)
	first := true
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		if first {
			first = false
			http.Error(w, "persist failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	releaser := NewCocoonNetLeaseReleaser(socketPath)
	if err := releaser.ReleaseByMAC(t.Context(), "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("a single 500 must be retried to success: %v", err)
	}
	if got := len(hits); got != 2 {
		t.Fatalf("requests = %d, want 2 (original + one retry)", got)
	}
}

func TestCocoonNetLeaseReleaserErrors(t *testing.T) {
	t.Run("invalid mac", func(t *testing.T) {
		releaser := NewCocoonNetLeaseReleaser(shortSocketPath(t))
		if err := releaser.ReleaseByMAC(t.Context(), "bad"); err == nil {
			t.Fatal("expected invalid MAC error")
		}
	})

	t.Run("server error", func(t *testing.T) {
		socketPath := shortSocketPath(t)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer listener.Close()
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})}
		go func() { _ = server.Serve(listener) }()
		defer server.Close()

		releaser := NewCocoonNetLeaseReleaser(socketPath)
		if err := releaser.ReleaseByMAC(t.Context(), "aa:bb:cc:dd:ee:ff"); err == nil {
			t.Fatal("expected server error")
		}
	})
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "vk-cocoon-net-*.sock")
	if err != nil {
		t.Fatalf("create socket path: %v", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatalf("close socket placeholder: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove socket placeholder: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}
