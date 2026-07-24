package snapshots

import (
	"testing"

	"github.com/cocoonstack/cocoon-common/manifest"
)

func TestSnapshotNeedsPullGate(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"schemaVersion":2,"artifactType":"` + manifest.ArtifactTypeSnapshotV2 + `"}`, true},
		{`{"schemaVersion":2,"artifactType":"` + manifest.ArtifactTypeSnapshot + `"}`, false},
		{`{"schemaVersion":2,"mediaType":"` + manifest.MediaTypeOCIIndex + `"}`, true},
		{`{"schemaVersion":2,"mediaType":"` + manifest.MediaTypeDockerIndex + `"}`, true},
		{`not json`, false},
	} {
		if got := snapshotNeedsPullGate([]byte(tc.raw)); got != tc.want {
			t.Errorf("snapshotNeedsPullGate(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
