package snapshots

import "testing"

func TestTransferConfigFromEnv(t *testing.T) {
	t.Setenv("SNAPSHOT_ZSTD_LEVEL", "3")
	t.Setenv("SNAPSHOT_CHUNK_SIZE_MIB", "512")
	t.Setenv("SNAPSHOT_TRANSFER_CONCURRENCY", "16")
	t.Setenv("SNAPSHOT_MEMORY_BUDGET_MIB", "4096")
	t.Setenv("SNAPSHOT_PULL_BUDGET_MIB", "2048")
	got := TransferConfigFromEnv()
	want := TransferConfig{
		ZstdLevel:       3,
		ChunkSizeMiB:    512,
		Concurrency:     16,
		MemoryBudgetMiB: 4096,
		PullBudgetMiB:   2048,
	}
	if got != want {
		t.Errorf("TransferConfigFromEnv() = %+v, want %+v", got, want)
	}
}

func TestTransferConfigFromEnvDefaultsOff(t *testing.T) {
	t.Setenv("SNAPSHOT_ZSTD_LEVEL", "")
	t.Setenv("SNAPSHOT_CHUNK_SIZE_MIB", "not-a-number")
	t.Setenv("SNAPSHOT_TRANSFER_CONCURRENCY", "-4")
	t.Setenv("SNAPSHOT_MEMORY_BUDGET_MIB", "")
	t.Setenv("SNAPSHOT_PULL_BUDGET_MIB", "-1")
	got := TransferConfigFromEnv()
	if got != (TransferConfig{}) {
		t.Errorf("unset/invalid env must keep the v1 writer, got %+v", got)
	}
}
