package vm

import (
	"errors"
	"reflect"
	"testing"
)

func TestIsCocoonNotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not not-found", err: nil, want: false},
		{name: "vm not found", err: errors.New("exit status 1 (stderr: vm not found)"), want: true},
		{name: "no such vm", err: errors.New("exit status 2 (stderr: no such vm)"), want: true},
		{name: "case-insensitive VM Not Found", err: errors.New("VM Not Found"), want: true},
		{name: "unrelated binary not found must not match", err: errors.New("exec: \"cocoon\": executable file not found in $PATH"), want: false},
		{name: "config file not found must not match", err: errors.New("config file not found"), want: false},
		{name: "transient sudo failure", err: errors.New("exec: sudo: broken pipe"), want: false},
		{name: "permission denied", err: errors.New("permission denied"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCocoonNotFound(tc.err); got != tc.want {
				t.Fatalf("isCocoonNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

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

func TestBuildCloneArgsStripsCPUAndMemoryForFirecracker(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts CloneOptions
		want []string
	}{
		{
			name: "cloud-hypervisor clone passes cpu/memory through",
			opts: CloneOptions{From: "snap-a", To: "vm-a", CPU: 2, Memory: "4Gi", Storage: "20Gi", Backend: "cloud-hypervisor"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-a", "--cpu", "2", "--memory", "4294967296", "--storage", "21474836480", "snap-a"},
		},
		{
			name: "firecracker clone drops cpu/memory but keeps storage",
			opts: CloneOptions{From: "snap-a", To: "vm-b", CPU: 2, Memory: "4Gi", Storage: "20Gi", Backend: "firecracker"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-b", "--storage", "21474836480", "snap-a"},
		},
		{
			name: "empty backend behaves like cloud-hypervisor",
			opts: CloneOptions{From: "snap-a", To: "vm-c", CPU: 1, Memory: "1Gi"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-c", "--cpu", "1", "--memory", "1073741824", "snap-a"},
		},
		{
			name: "no-direct-io flag appended when set",
			opts: CloneOptions{From: "snap-a", To: "vm-d", CPU: 1, Memory: "1Gi", NoDirectIO: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-d", "--cpu", "1", "--memory", "1073741824", "--no-direct-io", "snap-a"},
		},
		{
			name: "pull flag appended when set",
			opts: CloneOptions{From: "snap-a", To: "vm-e", CPU: 1, Memory: "1Gi", Pull: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-e", "--cpu", "1", "--memory", "1073741824", "--pull", "snap-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCloneArgs(tc.opts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildCloneArgs() = %#v, want %#v", got, tc.want)
			}
		})
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
			want: []string{"vm", "run", "--output", "json", "--name", "vm-a", "ghcr.io/x/y:1"},
		},
		{
			name: "firecracker adds --fc",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-b", OS: "linux", Backend: "firecracker"},
			want: []string{"vm", "run", "--output", "json", "--name", "vm-b", "--fc", "ghcr.io/x/y:1"},
		},
		{
			name: "windows adds --windows",
			opts: RunOptions{Image: "ghcr.io/x/w:1", Name: "vm-c", OS: "windows"},
			want: []string{"vm", "run", "--output", "json", "--name", "vm-c", "--windows", "ghcr.io/x/w:1"},
		},
		{
			name: "empty backend leaves --fc off",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-d", OS: "linux"},
			want: []string{"vm", "run", "--output", "json", "--name", "vm-d", "ghcr.io/x/y:1"},
		},
		{
			name: "no-direct-io flag appended when set",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-e", NoDirectIO: true},
			want: []string{"vm", "run", "--output", "json", "--name", "vm-e", "--no-direct-io", "ghcr.io/x/y:1"},
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
