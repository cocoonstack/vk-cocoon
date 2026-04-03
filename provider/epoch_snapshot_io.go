package provider

// snapshotExportEnvelope matches cocoon's types.SnapshotExport.
type snapshotExportEnvelope struct {
	Version int                  `json:"version"`
	Config  snapshotExportConfig `json:"config"`
}

type snapshotExportConfig struct {
	ID           string              `json:"id,omitempty"`
	Name         string              `json:"name"`
	Image        string              `json:"image,omitempty"`
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"`
	CPU          int                 `json:"cpu,omitempty"`
	Memory       int64               `json:"memory,omitempty"`
	Storage      int64               `json:"storage,omitempty"`
	NICs         int                 `json:"nics,omitempty"`
}

const snapshotJSONName = "snapshot.json"
