package snapshots

import (
	"context"

	"github.com/cocoonstack/epoch/snapshot"
)

// Registry abstracts the OCI backend (vs epoch's concrete *registryclient.Client)
// so a standard-OCI implementation can drop in. Beyond snapshot Uploader/Downloader
// it needs DeleteManifest for hibernate rollback.
type Registry interface {
	snapshot.Uploader
	snapshot.Downloader
	DeleteManifest(ctx context.Context, name, reference string) error
}
