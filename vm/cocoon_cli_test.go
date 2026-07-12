package vm

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/utils/ptr"
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
		{name: "broken pipe must not match", err: errors.New("exec: broken pipe"), want: false},
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

func TestIsCocoonSnapshotNotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "snapshot not found", err: errors.New("exit status 1 (stderr: snapshot not found)"), want: true},
		{name: "no such snapshot", err: errors.New("exit status 2 (stderr: no such snapshot)"), want: true},
		{name: "case-insensitive Snapshot Not Found", err: errors.New("Snapshot Not Found"), want: true},
		{name: "vm not found must not match", err: errors.New("vm not found"), want: false},
		{name: "config not found must not match", err: errors.New("config file not found"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCocoonSnapshotNotFound(tc.err); got != tc.want {
				t.Fatalf("isCocoonSnapshotNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBuildCloneArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts CloneOptions
		want []string
	}{
		{
			name: "default cloud-hypervisor clone keeps just name and positional snapshot",
			opts: CloneOptions{From: "snap-a", To: "vm-a", Backend: "cloud-hypervisor"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-a", "snap-a"},
		},
		{
			name: "firecracker clone strips on-demand",
			opts: CloneOptions{From: "snap-a", To: "vm-b", Backend: "firecracker", OnDemand: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-b", "snap-a"},
		},
		{
			name: "network flag passes through",
			opts: CloneOptions{From: "snap-a", To: "vm-c", Network: "cocoon-dhcp"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-c", "--network", "cocoon-dhcp", "snap-a"},
		},
		{
			name: "no-direct-io flag appended when set",
			opts: CloneOptions{From: "snap-a", To: "vm-d", NoDirectIO: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-d", "--no-direct-io", "snap-a"},
		},
		{
			name: "pull flag appended when set",
			opts: CloneOptions{From: "snap-a", To: "vm-e", Pull: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-e", "--pull", "snap-a"},
		},
		{
			name: "on-demand appended on cloud-hypervisor",
			opts: CloneOptions{From: "snap-a", To: "vm-od", Backend: "cloud-hypervisor", OnDemand: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-od", "--on-demand", "snap-a"},
		},
		{
			name: "from-dir replaces positional and forces --pull",
			opts: CloneOptions{To: "vm-f", FromDir: "/var/lib/cocoon/snaps/foo", Backend: "cloud-hypervisor"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-f", "--pull", "--from-dir", "/var/lib/cocoon/snaps/foo"},
		},
		{
			name: "from-dir on cloud-hypervisor with on-demand",
			opts: CloneOptions{To: "vm-g", FromDir: "/snaps/bar", Backend: "cloud-hypervisor", OnDemand: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-g", "--pull", "--on-demand", "--from-dir", "/snaps/bar"},
		},
		{
			name: "from-dir on firecracker skips on-demand",
			opts: CloneOptions{To: "vm-h", FromDir: "/snaps/fc", Backend: "firecracker", OnDemand: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-h", "--pull", "--from-dir", "/snaps/fc"},
		},
		{
			name: "from-dir suppresses positional From when both set",
			opts: CloneOptions{From: "ignored-snap", To: "vm-i", FromDir: "/snaps/baz"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-i", "--pull", "--from-dir", "/snaps/baz"},
		},
		{
			name: "from-dir emits --pull only once when caller also set Pull",
			opts: CloneOptions{To: "vm-j", FromDir: "/snaps/qux", Pull: true},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-j", "--pull", "--from-dir", "/snaps/qux"},
		},
		{
			name: "nics override appended before positional snapshot",
			opts: CloneOptions{From: "snap-a", To: "vm-k", NICs: ptr.To(1)},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-k", "--nics", "1", "snap-a"},
		},
		{
			name: "nics zero override emits --nics 0",
			opts: CloneOptions{From: "snap-a", To: "vm-l", NICs: ptr.To(0)},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-l", "--nics", "0", "snap-a"},
		},
		{
			name: "nics combines with on-demand on CH",
			opts: CloneOptions{From: "snap-a", To: "vm-m", Backend: "cloud-hypervisor", OnDemand: true, NICs: ptr.To(2)},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-m", "--on-demand", "--nics", "2", "snap-a"},
		},
		{
			name: "firecracker clone strips --nics",
			opts: CloneOptions{From: "snap-a", To: "vm-n", Backend: "firecracker", NICs: ptr.To(1)},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-n", "snap-a"},
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

func TestBuildRunArgs(t *testing.T) {
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
		{
			name: "k8s quantities normalized to byte counts",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-f", CPU: 2, Memory: "4Gi", Storage: "20Gi", Network: "cocoon-dhcp"},
			want: []string{
				"vm", "run", "--output", "json", "--name", "vm-f",
				"--cpu", "2", "--memory", "4294967296", "--storage", "21474836480",
				"--network", "cocoon-dhcp", "ghcr.io/x/y:1",
			},
		},
		{
			name: "zero cpu and empty quantities omit their flags",
			opts: RunOptions{Image: "ghcr.io/x/y:1", Name: "vm-g", Storage: "20Gi"},
			want: []string{"vm", "run", "--output", "json", "--name", "vm-g", "--storage", "21474836480", "ghcr.io/x/y:1"},
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

func TestBuildExecArgsAssemblesEnvAndArgv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		vmID        string
		argv        []string
		env         map[string]string
		interactive bool
		want        []string
	}{
		{
			name: "no env passes through plain argv",
			vmID: "vm-1",
			argv: []string{"echo", "hello world"},
			want: []string{"vm", "exec", "vm-1", "--", "echo", "hello world"},
		},
		{
			name: "env keys sorted for deterministic argv",
			vmID: "vm-2",
			argv: []string{"printenv"},
			env:  map[string]string{"FOO": "1", "BAR": "2", "BAZ": "3"},
			want: []string{"vm", "exec", "-e", "BAR=2", "-e", "BAZ=3", "-e", "FOO=1", "vm-2", "--", "printenv"},
		},
		{
			name: "argv with leading -- is preserved (cocoon swallows the first one)",
			vmID: "vm-3",
			argv: []string{"--", "ls"},
			want: []string{"vm", "exec", "vm-3", "--", "--", "ls"},
		},
		{
			name:        "interactive adds -i before env",
			vmID:        "vm-4",
			argv:        []string{"cat"},
			env:         map[string]string{"FOO": "1"},
			interactive: true,
			want:        []string{"vm", "exec", "-i", "-e", "FOO=1", "vm-4", "--", "cat"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildExecArgs(tc.vmID, tc.argv, tc.env, tc.interactive)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildExecArgs() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
