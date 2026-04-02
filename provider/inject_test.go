package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForSSHEventuallySucceeds(t *testing.T) {
	oldProbe := sshReadyProbe
	oldInterval := sshReadyPollInterval
	defer func() {
		sshReadyProbe = oldProbe
		sshReadyPollInterval = oldInterval
	}()

	sshReadyPollInterval = 0
	attempts := 0
	sshReadyProbe = func(_ context.Context, vm *CocoonVM, password string) error {
		attempts++
		if vm.ip != "10.88.100.10" {
			t.Fatalf("vm.ip = %q, want 10.88.100.10", vm.ip)
		}
		if password != "secret" {
			t.Fatalf("password = %q, want secret", password)
		}
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	}

	vm := &CocoonVM{ip: "10.88.100.10"}
	if err := (guestExecutor{}).waitForSSH(context.Background(), vm, "secret", time.Second); err != nil {
		t.Fatalf("waitForSSH: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWaitForSSHTimesOut(t *testing.T) {
	oldProbe := sshReadyProbe
	oldInterval := sshReadyPollInterval
	defer func() {
		sshReadyProbe = oldProbe
		sshReadyPollInterval = oldInterval
	}()

	sshReadyPollInterval = 0
	sshReadyProbe = func(context.Context, *CocoonVM, string) error {
		return errors.New("connection refused")
	}

	vm := &CocoonVM{ip: "10.88.100.11"}
	if err := (guestExecutor{}).waitForSSH(context.Background(), vm, "secret", 10*time.Millisecond); err == nil {
		t.Fatal("waitForSSH: expected timeout error")
	}
}
