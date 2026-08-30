package snapshots

import commonk8s "github.com/cocoonstack/cocoon-common/k8s"

// TransferConfig carries the snapshot wire-format knobs; all-zero keeps the v1 writer.
type TransferConfig struct {
	ZstdLevel       int // 0 = no compression
	ChunkSizeMiB    int // 0 = no chunking
	Concurrency     int // 0 = library default (8)
	MemoryBudgetMiB int // push buffer cap, 0 = library default (9216)
	PullBudgetMiB   int // pull prefetch-window cap per pull, 0 = defaultPullBudgetMiB
}

// TransferConfigFromEnv reads the SNAPSHOT_* knobs; unset or invalid values become 0.
func TransferConfigFromEnv() TransferConfig {
	return TransferConfig{
		ZstdLevel:       envInt("SNAPSHOT_ZSTD_LEVEL"),
		ChunkSizeMiB:    envInt("SNAPSHOT_CHUNK_SIZE_MIB"),
		Concurrency:     envInt("SNAPSHOT_TRANSFER_CONCURRENCY"),
		MemoryBudgetMiB: envInt("SNAPSHOT_MEMORY_BUDGET_MIB"),
		PullBudgetMiB:   envInt("SNAPSHOT_PULL_BUDGET_MIB"),
	}
}

func envInt(key string) int {
	return max(commonk8s.EnvInt(key, 0), 0)
}
