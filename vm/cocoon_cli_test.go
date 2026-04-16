package vm

import (
	"reflect"
	"testing"
)

func TestAppendCreateArgsNormalizesResourceQuantities(t *testing.T) {
	t.Parallel()

	got := appendCreateArgs([]string{"vm", "run"}, 2, "4Gi", "cocoon-dhcp", "20Gi", 0, nil)
	want := []string{
		"vm", "run",
		"--cpu", "2",
		"--memory", "4294967296",
		"--storage", "21474836480",
		"--network", "cocoon-dhcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendCreateArgs() = %#v, want %#v", got, want)
	}
}

func TestAppendCreateArgsKeepsEmptyInheritedValues(t *testing.T) {
	t.Parallel()

	got := appendCreateArgs([]string{"vm", "clone"}, 0, "", "", "20Gi", 0, nil)
	want := []string{
		"vm", "clone",
		"--storage", "21474836480",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendCreateArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildRunArgsAppendsBackendAndOSFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts RunOptions
		want []string
	}{
		{
			name: "cloud-hypervisor linux (default) skips --fc and --windows",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-a", OS: "linux", Backend: "cloud-hypervisor"},
			want: []string{"vm", "run", "--name", "vm-a", "ghcr.io/x/y:1"},
		},
		{
			name: "firecracker adds --fc",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-b", OS: "linux", Backend: "firecracker"},
			want: []string{"vm", "run", "--name", "vm-b", "--fc", "ghcr.io/x/y:1"},
		},
		{
			name: "windows adds --windows",
			opts: RunOptions{Image: "ghcr.io/x/w:1", Name: "vm-c", OS: "windows"},
			want: []string{"vm", "run", "--name", "vm-c", "--windows", "ghcr.io/x/w:1"},
		},
		{
			name: "empty backend leaves --fc off",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-d", OS: "linux"},
			want: []string{"vm", "run", "--name", "vm-d", "ghcr.io/x/y:1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRunArgs(tc.opts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildRunArgs() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
