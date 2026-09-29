package cocoon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/ociutil"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestTheReclaimerDropsOnlyWhatNoPodOnTheNodeCanWakeFrom(t *testing.T) {
	const vmName = "vk-ns-demo-0-505043"
	importName := vmName + meta.HibernateImportSuffix
	self := meta.VMSpec{VMName: vmName}
	cloner := meta.VMSpec{VMName: "vk-ns-clone-0-9a1f3c", Image: vmName}
	taggedCopy := vmName + ":v2"
	tagCloner := meta.VMSpec{VMName: "vk-ns-clone-0-9a1f3c", Image: taggedCopy}
	digestRef := vmName + "@sha256:" + strings.Repeat("ab", 32)
	digestCopy := localSnapshotName(ociutil.ParseRef(digestRef))
	digestCloner := meta.VMSpec{VMName: "vk-ns-clone-0-9a1f3c", Image: digestRef}
	tests := []struct {
		name     string
		local    string
		pod      meta.VMSpec
		registry oci.Registry
		want     []string
	}{
		{"a copy whose tag is gone", vmName, meta.VMSpec{}, tagGone(t), []string{forkSnapshotName(vmName), vmName}},
		{"a copy whose tag names another snapshot", vmName, meta.VMSpec{}, newWakeVerifyRegistry(t, "SNAP-2"), []string{forkSnapshotName(vmName), vmName}},
		{"a copy the tag still names", vmName, meta.VMSpec{}, newWakeVerifyRegistry(t, "SNAP-1"), nil},
		{"a registry that cannot answer", vmName, meta.VMSpec{}, registryDown(t), nil},
		{"a copy a pod on this node tracks", vmName, self, tagGone(t), nil},
		{"a copy a pod on this node clones from", vmName, cloner, tagGone(t), nil},
		{"a snapshot not named for a VM", "ubuntu-base", meta.VMSpec{}, tagGone(t), nil},
		{"an import no pod on this node wakes", importName, meta.VMSpec{}, newWakeVerifyRegistry(t, "SNAP-1"), []string{importName}},
		{"an import a pod on this node wakes", importName, self, newWakeVerifyRegistry(t, "SNAP-1"), nil},
		{"no registry configured", vmName, meta.VMSpec{}, nil, nil},
		{"a tagged clone-source copy no pod clones from", taggedCopy, meta.VMSpec{}, registryDown(t), []string{taggedCopy}},
		{"a tagged clone-source copy a pod clones from", taggedCopy, tagCloner, registryDown(t), nil},
		{"a digest clone-source copy no pod clones from", digestCopy, meta.VMSpec{}, registryDown(t), []string{digestCopy}},
		{"a digest clone-source copy a pod clones from", digestCopy, digestCloner, registryDown(t), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{tt.local: {Name: tt.local, ID: "SNAP-1"}}}
			p := newTestProvider(t)
			p.Runtime = rt
			p.Registry = tt.registry
			if tt.pod.VMName != "" {
				p.trackPod(newPodWithSpec(tt.pod), nil)
			}

			p.reclaimLocalSnapshots(t.Context())

			if got := slices.Sorted(slices.Values(rt.snapshotRemoveCalls)); !slices.Equal(got, tt.want) {
				t.Errorf("removed %v, want %v", got, tt.want)
			}
		})
	}
}

func tagGone(t *testing.T) oci.Registry {
	r := newWakeVerifyRegistry(t, "SNAP-1")
	r.tagExists = false
	return r
}

func registryDown(t *testing.T) oci.Registry {
	r := newWakeVerifyRegistry(t, "SNAP-1")
	r.manifestErr = errors.New("registry unreachable")
	return r
}
