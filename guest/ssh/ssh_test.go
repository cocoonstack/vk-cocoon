package ssh

import (
	"testing"
)

func TestNewExecutorDefaults(t *testing.T) {
	e := NewExecutor("root", "pw", 0)
	if e.Port != defaultPort {
		t.Errorf("Port = %d, want %d", e.Port, defaultPort)
	}
	if e.DialTimeout != defaultDialTimeout {
		t.Errorf("DialTimeout = %v, want %v", e.DialTimeout, defaultDialTimeout)
	}
	if e.User != "root" || e.Password != "pw" {
		t.Errorf("user/password not captured: %+v", e)
	}
}
