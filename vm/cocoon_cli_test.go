package vm

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestSnapshotNameTakenPhrases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "preflight rejection",
			out:  `Error: snapshot name "vk-ns-demo-0" already exists`,
			want: true,
		},
		{
			// The store's name index also sees the pending record a killed save leaves.
			name: "name index rejection",
			out:  `Error: save snapshot: snapshot name "vk-ns-demo-0" already in use by MPT5A6ZS2FNZWQGFN24AZLREWQ`,
			want: true,
		},
		{name: "unrelated failure", out: "Error: vm is paused (snapshot or hibernate in flight)", want: false},
		{name: "disk full", out: "Error: save snapshot: write data: no space left on device", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsAny(tc.out, snapshotNameTaken...); got != tc.want {
				t.Fatalf("containsAny(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestSnapshotNameHolderID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "name index rejection",
			out:  `Error: save snapshot: snapshot name "vk-ns-demo-0" already in use by MPT5A6ZS2FNZWQGFN24AZLREWQ`,
			want: "MPT5A6ZS2FNZWQGFN24AZLREWQ",
		},
		{
			name: "preflight rejection names the holder",
			out:  `Error: snapshot name "vk-ns-demo-0" already exists (held by MPT5A6ZS2FNZWQGFN24AZLREWQ)`,
			want: "MPT5A6ZS2FNZWQGFN24AZLREWQ",
		},
		{
			// Older cocoon names no holder; the caller falls back to the name.
			name: "preflight rejection without a holder",
			out:  `Error: snapshot name "vk-ns-demo-0" already exists`,
			want: "",
		},
		{name: "unrelated output", out: "Error: no space left on device", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotNameHolderID(tc.out); got != tc.want {
				t.Fatalf("snapshotNameHolderID(%q) = %q, want %q", tc.out, got, tc.want)
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
			name: "firecracker clone strips restore-mode",
			opts: CloneOptions{From: "snap-a", To: "vm-b", Backend: "firecracker", RestoreMode: RestoreOnDemand},
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
			name: "restore-mode ondemand appended on cloud-hypervisor",
			opts: CloneOptions{From: "snap-a", To: "vm-od", Backend: "cloud-hypervisor", RestoreMode: RestoreOnDemand},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-od", "--restore-mode", "ondemand", "snap-a"},
		},
		{
			name: "restore-mode mmap appended on cloud-hypervisor",
			opts: CloneOptions{From: "snap-a", To: "vm-mm", Backend: "cloud-hypervisor", RestoreMode: RestoreMmap},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-mm", "--restore-mode", "mmap", "snap-a"},
		},
		{
			name: "restore-mode copy is spelled out, not left to cocoon's mmap default",
			opts: CloneOptions{From: "snap-a", To: "vm-cp", Backend: "cloud-hypervisor", RestoreMode: RestoreCopy},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-cp", "--restore-mode", "copy", "snap-a"},
		},
		{
			name: "empty backend treated as cloud-hypervisor for restore-mode",
			opts: CloneOptions{From: "snap-a", To: "vm-eb", RestoreMode: RestoreOnDemand},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-eb", "--restore-mode", "ondemand", "snap-a"},
		},
		{
			name: "from-dir replaces positional and forces --pull",
			opts: CloneOptions{To: "vm-f", FromDir: "/var/lib/cocoon/snaps/foo", Backend: "cloud-hypervisor"},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-f", "--pull", "--from-dir", "/var/lib/cocoon/snaps/foo"},
		},
		{
			name: "from-dir on cloud-hypervisor with restore-mode",
			opts: CloneOptions{To: "vm-g", FromDir: "/snaps/bar", Backend: "cloud-hypervisor", RestoreMode: RestoreOnDemand},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-g", "--pull", "--restore-mode", "ondemand", "--from-dir", "/snaps/bar"},
		},
		{
			name: "from-dir on firecracker skips restore-mode",
			opts: CloneOptions{To: "vm-h", FromDir: "/snaps/fc", Backend: "firecracker", RestoreMode: RestoreOnDemand},
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
			name: "nics combines with restore-mode on CH",
			opts: CloneOptions{From: "snap-a", To: "vm-m", Backend: "cloud-hypervisor", RestoreMode: RestoreOnDemand, NICs: ptr.To(2)},
			want: []string{"vm", "clone", "--output", "json", "--name", "vm-m", "--restore-mode", "ondemand", "--nics", "2", "snap-a"},
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

func TestParseRestoreMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    RestoreMode
		wantErr bool
	}{
		{in: "copy", want: RestoreCopy},
		{in: "ondemand", want: RestoreOnDemand},
		{in: "mmap", want: RestoreMmap},
		{in: "MMAP", want: RestoreMmap},
		{in: " ondemand ", want: RestoreOnDemand},
		{in: "", wantErr: true},
		{in: "on-demand", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(cmp.Or(tc.in, "empty"), func(t *testing.T) {
			got, err := ParseRestoreMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRestoreMode(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRestoreMode(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRestoreMode(%q) = %q, want %q", tc.in, got, tc.want)
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

func TestRemoveStaleSnapshotWaitsOutHeldLease(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cocoon")
	// Refuses while the lease is held, then succeeds — the orphaned child dying.
	payload := `#!/bin/sh
[ -f ` + dir + `/done ] && exit 0
touch ` + dir + `/done
echo 'Error: rm: snapshot XY7T3JIQHJ25KUVDJCJK5V3OSL is in use by an active clone/restore/export'
exit 1
`
	if err := os.WriteFile(script, []byte(payload), 0o755); err != nil {
		t.Fatalf("write fake cocoon: %v", err)
	}
	if err := NewCocoonCLI(script).removeStaleSnapshot(t.Context(), "XY7T3JIQHJ25KUVDJCJK5V3OSL"); err != nil {
		t.Fatalf("removeStaleSnapshot: %v", err)
	}
}

func TestRemoveStaleSnapshotDoesNotRetryOtherErrors(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cocoon")
	payload := `#!/bin/sh
[ -f ` + dir + `/called ] && { echo 'Error: retried a terminal failure'; exit 1; }
touch ` + dir + `/called
echo 'Error: rm: remove data dir: input/output error'
exit 1
`
	if err := os.WriteFile(script, []byte(payload), 0o755); err != nil {
		t.Fatalf("write fake cocoon: %v", err)
	}
	err := NewCocoonCLI(script).removeStaleSnapshot(t.Context(), "XY7T3JIQHJ25KUVDJCJK5V3OSL")
	if err == nil {
		t.Fatal("a non-lease rm failure must be reported, not retried")
	}
	if !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("error = %v, want the first attempt's output, not a retry", err)
	}
}

func TestWatchEventsSkipsUndecodableLine(t *testing.T) {
	script := filepath.Join(t.TempDir(), "cocoon")
	payload := `#!/bin/sh
echo '{"event":"ADDED","vm":{"id":"vm-1","config":{"name":"a"}}}'
echo 'not-json {{{'
echo '{"event":"DELETED","vm":{"id":"vm-2","config":{"name":"b"}}}'
`
	if err := os.WriteFile(script, []byte(payload), 0o755); err != nil {
		t.Fatalf("write fake cocoon: %v", err)
	}
	events, err := NewCocoonCLI(script).WatchEvents(t.Context())
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	var got []VMEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].VM.ID != "vm-1" || got[1].VM.ID != "vm-2" {
		t.Fatalf("events = %+v, want vm-1 then vm-2 with the torn line skipped", got)
	}
}
