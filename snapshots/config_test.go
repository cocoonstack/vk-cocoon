package snapshots

import "testing"

func TestTransferConfigFromEnv(t *testing.T) {
	t.Setenv("SNAPSHOT_ZSTD_LEVEL", "3")
	t.Setenv("SNAPSHOT_CHUNK_SIZE_MIB", "512")
	t.Setenv("SNAPSHOT_TRANSFER_CONCURRENCY", "16")
	t.Setenv("SNAPSHOT_MEMORY_BUDGET_MIB", "4096")
	got := TransferConfigFromEnv()
	if got.ZstdLevel != 3 || got.ChunkSizeMiB != 512 || got.Concurrency != 16 || got.MemoryBudgetMiB != 4096 {
		t.Errorf("TransferConfigFromEnv() = %+v", got)
	}
}

func TestTransferConfigFromEnvDefaultsOff(t *testing.T) {
	t.Setenv("SNAPSHOT_ZSTD_LEVEL", "")
	t.Setenv("SNAPSHOT_CHUNK_SIZE_MIB", "not-a-number")
	t.Setenv("SNAPSHOT_TRANSFER_CONCURRENCY", "-4")
	got := TransferConfigFromEnv()
	if got != (TransferConfig{}) {
		t.Errorf("unset/invalid env must keep the v1 writer, got %+v", got)
	}
}
