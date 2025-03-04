package bloomshipper

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/storage"
	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/chunk/cache"
	storageconfig "github.com/grafana/loki/pkg/storage/config"
	"github.com/grafana/loki/pkg/storage/stores/shipper/bloomshipper/config"
)

func newMockBloomStore(t *testing.T) (*BloomStore, string, error) {
	workDir := t.TempDir()
	return newMockBloomStoreWithWorkDir(t, workDir)
}

func newMockBloomStoreWithWorkDir(t *testing.T, workDir string) (*BloomStore, string, error) {
	periodicConfigs := []storageconfig.PeriodConfig{
		{
			ObjectType: storageconfig.StorageTypeInMemory,
			From:       parseDayTime("2024-01-01"),
			IndexTables: storageconfig.IndexPeriodicTableConfig{
				PeriodicTableConfig: storageconfig.PeriodicTableConfig{
					Period: 24 * time.Hour,
					Prefix: "schema_a_table_",
				}},
		},
		{
			ObjectType: storageconfig.StorageTypeInMemory,
			From:       parseDayTime("2024-02-01"),
			IndexTables: storageconfig.IndexPeriodicTableConfig{
				PeriodicTableConfig: storageconfig.PeriodicTableConfig{
					Period: 24 * time.Hour,
					Prefix: "schema_b_table_",
				}},
		},
	}

	storageConfig := storage.Config{
		BloomShipperConfig: config.Config{
			WorkingDirectory: workDir,
			BlocksDownloadingQueue: config.DownloadingQueueConfig{
				WorkersCount: 1,
			},
			BlocksCache: cache.EmbeddedCacheConfig{
				MaxSizeItems: 1000,
				TTL:          1 * time.Hour,
			},
		},
	}

	metrics := storage.NewClientMetrics()
	t.Cleanup(metrics.Unregister)
	logger := log.NewLogfmtLogger(os.Stderr)

	metasCache := cache.NewMockCache()
	blocksCache := NewBlocksCache(storageConfig.BloomShipperConfig.BlocksCache, prometheus.NewPedanticRegistry(), logger)

	store, err := NewBloomStore(periodicConfigs, storageConfig, metrics, metasCache, blocksCache, logger)
	if err == nil {
		t.Cleanup(store.Stop)
	}

	return store, workDir, err
}

func createMetaInStorage(store *BloomStore, tenant string, start model.Time, minFp, maxFp model.Fingerprint) (Meta, error) {
	meta := Meta{
		MetaRef: MetaRef{
			Ref: Ref{
				TenantID: tenant,
				Bounds:   v1.NewBounds(minFp, maxFp),
				// Unused
				// StartTimestamp: start,
				// EndTimestamp:   start.Add(12 * time.Hour),
			},
		},
		Blocks: []BlockRef{},
	}
	err := store.storeDo(start, func(s *bloomStoreEntry) error {
		raw, _ := json.Marshal(meta)
		meta.MetaRef.Ref.TableName = tablesForRange(s.cfg, NewInterval(start, start.Add(12*time.Hour)))[0]
		return s.objectClient.PutObject(context.Background(), s.Meta(meta.MetaRef).Addr(), bytes.NewReader(raw))
	})
	return meta, err
}

func createBlockInStorage(t *testing.T, store *BloomStore, tenant string, start model.Time, minFp, maxFp model.Fingerprint) (Block, error) {
	tmpDir := t.TempDir()
	fp, _ := os.CreateTemp(t.TempDir(), "*.tar.gz")

	blockWriter := v1.NewDirectoryBlockWriter(tmpDir)
	err := blockWriter.Init()
	require.NoError(t, err)

	err = v1.TarGz(fp, v1.NewDirectoryBlockReader(tmpDir))
	require.NoError(t, err)

	_, _ = fp.Seek(0, 0)

	block := Block{
		BlockRef: BlockRef{
			Ref: Ref{
				TenantID:       tenant,
				Bounds:         v1.NewBounds(minFp, maxFp),
				StartTimestamp: start,
				EndTimestamp:   start.Add(12 * time.Hour),
			},
		},
		Data: fp,
	}
	err = store.storeDo(start, func(s *bloomStoreEntry) error {
		block.BlockRef.Ref.TableName = tablesForRange(s.cfg, NewInterval(start, start.Add(12*time.Hour)))[0]
		return s.objectClient.PutObject(context.Background(), s.Block(block.BlockRef).Addr(), block.Data)
	})
	return block, err
}

func TestBloomStore_ResolveMetas(t *testing.T) {
	store, _, err := newMockBloomStore(t)
	require.NoError(t, err)

	// schema 1
	// outside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-19 00:00"), 0x00010000, 0x0001ffff)
	// outside of interval, inside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-19 00:00"), 0x00000000, 0x0000ffff)
	// inside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-20 00:00"), 0x00010000, 0x0001ffff)
	// inside of interval, inside of bounds
	m1, _ := createMetaInStorage(store, "tenant", parseTime("2024-01-20 00:00"), 0x00000000, 0x0000ffff)

	// schema 2
	// inside of interval, inside of bounds
	m2, _ := createMetaInStorage(store, "tenant", parseTime("2024-02-05 00:00"), 0x00000000, 0x0000ffff)
	// inside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-05 00:00"), 0x00010000, 0x0001ffff)
	// outside of interval, inside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-11 00:00"), 0x00000000, 0x0000ffff)
	// outside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-11 00:00"), 0x00010000, 0x0001ffff)

	t.Run("tenant matches", func(t *testing.T) {
		ctx := context.Background()
		params := MetaSearchParams{
			"tenant",
			NewInterval(parseTime("2024-01-20 00:00"), parseTime("2024-02-10 00:00")),
			v1.NewBounds(0x00000000, 0x0000ffff),
		}

		refs, fetchers, err := store.ResolveMetas(ctx, params)
		require.NoError(t, err)
		require.Len(t, refs, 2)
		require.Len(t, fetchers, 2)

		require.Equal(t, [][]MetaRef{{m1.MetaRef}, {m2.MetaRef}}, refs)
	})

	t.Run("tenant does not match", func(t *testing.T) {
		ctx := context.Background()
		params := MetaSearchParams{
			"other",
			NewInterval(parseTime("2024-01-20 00:00"), parseTime("2024-02-10 00:00")),
			v1.NewBounds(0x00000000, 0x0000ffff),
		}

		refs, fetchers, err := store.ResolveMetas(ctx, params)
		require.NoError(t, err)
		require.Len(t, refs, 0)
		require.Len(t, fetchers, 0)
		require.Equal(t, [][]MetaRef{}, refs)
	})
}

func TestBloomStore_FetchMetas(t *testing.T) {
	store, _, err := newMockBloomStore(t)
	require.NoError(t, err)

	// schema 1
	// outside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-19 00:00"), 0x00010000, 0x0001ffff)
	// outside of interval, inside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-19 00:00"), 0x00000000, 0x0000ffff)
	// inside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-01-20 00:00"), 0x00010000, 0x0001ffff)
	// inside of interval, inside of bounds
	m1, _ := createMetaInStorage(store, "tenant", parseTime("2024-01-20 00:00"), 0x00000000, 0x0000ffff)

	// schema 2
	// inside of interval, inside of bounds
	m2, _ := createMetaInStorage(store, "tenant", parseTime("2024-02-05 00:00"), 0x00000000, 0x0000ffff)
	// inside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-05 00:00"), 0x00010000, 0x0001ffff)
	// outside of interval, inside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-11 00:00"), 0x00000000, 0x0000ffff)
	// outside of interval, outside of bounds
	_, _ = createMetaInStorage(store, "tenant", parseTime("2024-02-11 00:00"), 0x00010000, 0x0001ffff)

	t.Run("tenant matches", func(t *testing.T) {
		ctx := context.Background()
		params := MetaSearchParams{
			"tenant",
			NewInterval(parseTime("2024-01-20 00:00"), parseTime("2024-02-10 00:00")),
			v1.NewBounds(0x00000000, 0x0000ffff),
		}

		metas, err := store.FetchMetas(ctx, params)
		require.NoError(t, err)
		require.Len(t, metas, 2)

		require.Equal(t, []Meta{m1, m2}, metas)
	})

	t.Run("tenant does not match", func(t *testing.T) {
		ctx := context.Background()
		params := MetaSearchParams{
			"other",
			NewInterval(parseTime("2024-01-20 00:00"), parseTime("2024-02-10 00:00")),
			v1.NewBounds(0x00000000, 0x0000ffff),
		}

		metas, err := store.FetchMetas(ctx, params)
		require.NoError(t, err)
		require.Len(t, metas, 0)
		require.Equal(t, []Meta{}, metas)
	})
}

func TestBloomStore_FetchBlocks(t *testing.T) {
	store, _, err := newMockBloomStore(t)
	require.NoError(t, err)

	// schema 1
	b1, _ := createBlockInStorage(t, store, "tenant", parseTime("2024-01-20 00:00"), 0x00000000, 0x0000ffff)
	b2, _ := createBlockInStorage(t, store, "tenant", parseTime("2024-01-20 00:00"), 0x00010000, 0x0001ffff)
	// schema 2
	b3, _ := createBlockInStorage(t, store, "tenant", parseTime("2024-02-05 00:00"), 0x00000000, 0x0000ffff)
	b4, _ := createBlockInStorage(t, store, "tenant", parseTime("2024-02-05 00:00"), 0x00000000, 0x0001ffff)

	ctx := context.Background()

	// first call fetches two blocks from cache
	bqs, err := store.FetchBlocks(ctx, []BlockRef{b1.BlockRef, b3.BlockRef})
	require.NoError(t, err)
	require.Len(t, bqs, 2)

	require.Equal(t, []BlockRef{b1.BlockRef, b3.BlockRef}, []BlockRef{bqs[0].BlockRef, bqs[1].BlockRef})

	// second call fetches two blocks from cache and two from storage
	bqs, err = store.FetchBlocks(ctx, []BlockRef{b1.BlockRef, b2.BlockRef, b3.BlockRef, b4.BlockRef})
	require.NoError(t, err)
	require.Len(t, bqs, 4)

	require.Equal(t,
		[]BlockRef{b1.BlockRef, b2.BlockRef, b3.BlockRef, b4.BlockRef},
		[]BlockRef{bqs[0].BlockRef, bqs[1].BlockRef, bqs[2].BlockRef, bqs[3].BlockRef},
	)
}

func TestBloomShipper_WorkingDir(t *testing.T) {
	t.Run("insufficient permissions on directory yields error", func(t *testing.T) {
		base := t.TempDir()
		wd := filepath.Join(base, "notpermitted")
		err := os.MkdirAll(wd, 0500)
		require.NoError(t, err)
		fi, _ := os.Stat(wd)
		t.Log("working directory", wd, fi.Mode())

		_, _, err = newMockBloomStoreWithWorkDir(t, wd)
		require.ErrorContains(t, err, "insufficient permissions")
	})

	t.Run("not existing directory will be created", func(t *testing.T) {
		base := t.TempDir()
		// if the base directory does not exist, it will be created
		wd := filepath.Join(base, "doesnotexist")
		t.Log("working directory", wd)

		store, _, err := newMockBloomStoreWithWorkDir(t, wd)
		require.NoError(t, err)
		b, err := createBlockInStorage(t, store, "tenant", parseTime("2024-01-20 00:00"), 0x00000000, 0x0000ffff)
		require.NoError(t, err)

		ctx := context.Background()
		_, err = store.FetchBlocks(ctx, []BlockRef{b.BlockRef})
		require.NoError(t, err)
	})
}
