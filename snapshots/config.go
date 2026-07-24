package snapshots

import (
	"os"
	"strconv"
)

// TransferConfig carries the snapshot wire-format knobs; all-zero keeps the v1 writer.
type TransferConfig struct {
	ZstdLevel       int // 0 = no compression
	ChunkSizeMiB    int // 0 = no chunking
	Concurrency     int // 0 = library default (8)
	MemoryBudgetMiB int // push buffer cap, 0 = library default (9216)
}

// TransferConfigFromEnv reads the SNAPSHOT_* knobs; unset or invalid values become 0.
func TransferConfigFromEnv() TransferConfig {
	return TransferConfig{
		ZstdLevel:       envInt("SNAPSHOT_ZSTD_LEVEL"),
		ChunkSizeMiB:    envInt("SNAPSHOT_CHUNK_SIZE_MIB"),
		Concurrency:     envInt("SNAPSHOT_TRANSFER_CONCURRENCY"),
		MemoryBudgetMiB: envInt("SNAPSHOT_MEMORY_BUDGET_MIB"),
	}
}

func envInt(key string) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v < 0 {
		return 0
	}
	return v
}
