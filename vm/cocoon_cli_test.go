package vm

import (
	"reflect"
	"testing"
)

func TestAppendCreateArgsNormalizesResourceQuantities(t *testing.T) {
	t.Parallel()

	got := appendCreateArgs([]string{"vm", "run"}, 2, "4Gi", "dnsmasq-dhcp", "20Gi", 0, nil)
	want := []string{
		"vm", "run",
		"--cpu", "2",
		"--memory", "4294967296",
		"--storage", "21474836480",
		"--network", "dnsmasq-dhcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendCreateArgs() = %#v, want %#v", got, want)
	}
}

func TestAppendCloneArgsKeepsEmptyInheritedValues(t *testing.T) {
	t.Parallel()

	got := appendCloneArgs([]string{"vm", "clone"}, 0, "", "", "20Gi", 0, nil)
	want := []string{
		"vm", "clone",
		"--storage", "21474836480",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendCloneArgs() = %#v, want %#v", got, want)
	}
}
