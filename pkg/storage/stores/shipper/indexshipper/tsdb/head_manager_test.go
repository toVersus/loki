package tsdb

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/stretchr/testify/require"

	"github.com/grafana/dskit/flagext"

	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/storage/chunk"
	"github.com/grafana/loki/v3/pkg/storage/chunk/client/local"
	"github.com/grafana/loki/v3/pkg/storage/chunk/client/util"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/tsdb/index"
	"github.com/grafana/loki/v3/pkg/storage/types"
	"github.com/grafana/loki/v3/pkg/validation"
)

type noopTSDBManager struct {
	name string
	dir  string
	*tenantHeads
}

func newNoopTSDBManager(name, dir string) noopTSDBManager {
	return noopTSDBManager{
		name:        name,
		dir:         dir,
		tenantHeads: newTenantHeads(time.Now(), defaultHeadManagerStripeSize, NewMetrics(nil), log.NewNopLogger()),
	}
}

func (m noopTSDBManager) BuildFromHead(_ *tenantHeads) error {
	return nil
}

func (m noopTSDBManager) BuildFromWALs(_ time.Time, wals []WALIdentifier, _ bool) error {
	return recoverHead(m.name, m.dir, m.tenantHeads, wals, false, log.NewNopLogger(), NewMetrics(nil).walCorruptionsRepairs)
}
func (m noopTSDBManager) Start() error { return nil }

type zeroValueLimits struct {
}

func (m *zeroValueLimits) AllByUserID() map[string]*validation.Limits {
	return nil
}

func (m *zeroValueLimits) VolumeMaxSeries(_ string) int {
	return 0
}

func (m *zeroValueLimits) DefaultLimits() *validation.Limits {
	return &validation.Limits{
		QueryReadyIndexNumDays: 0,
	}
}

func chunkMetasToChunkRefs(user string, fp uint64, xs index.ChunkMetas) (res []ChunkRef) {
	for _, x := range xs {
		res = append(res, ChunkRef{
			User:        user,
			Fingerprint: model.Fingerprint(fp),
			Start:       x.From(),
			End:         x.Through(),
			Checksum:    x.Checksum,
		})
	}
	return
}

func chunkMetasToLogProtoChunkRefs(user string, fp uint64, xs index.ChunkMetas) (res []logproto.ChunkRef) {
	for _, x := range xs {
		res = append(res, logproto.ChunkRef{
			UserID:      user,
			Fingerprint: fp,
			From:        model.TimeFromUnix(x.From().Unix()),
			Through:     model.TimeFromUnix(x.Through().Unix()),
			Checksum:    x.Checksum,
		})
	}
	return
}

// Test append
func Test_TenantHeads_Append(t *testing.T) {
	h := newTenantHeads(time.Now(), defaultHeadManagerStripeSize, NewMetrics(nil), log.NewNopLogger())
	ls := mustParseLabels(`{foo="bar"}`)
	chks := []index.ChunkMeta{
		{
			Checksum: 0,
			MinTime:  1,
			MaxTime:  10,
			KB:       2,
			Entries:  30,
		},
	}
	_ = h.Append("fake", ls, labels.StableHash(ls), chks)

	found, err := h.GetChunkRefs(
		context.Background(),
		"fake",
		0,
		100,
		nil, nil,
		labels.MustNewMatcher(labels.MatchEqual, "foo", "bar"),
	)
	require.Nil(t, err)
	require.Equal(t, chunkMetasToChunkRefs("fake", labels.StableHash(ls), chks), found)

}

// Test multitenant reads
func Test_TenantHeads_MultiRead(t *testing.T) {
	h := newTenantHeads(time.Now(), defaultHeadManagerStripeSize, NewMetrics(nil), log.NewNopLogger())
	ls := mustParseLabels(`{foo="bar"}`)
	chks := []index.ChunkMeta{
		{
			Checksum: 0,
			MinTime:  1,
			MaxTime:  10,
			KB:       2,
			Entries:  30,
		},
	}

	tenants := []struct {
		user string
		ls   labels.Labels
	}{
		{
			user: "tenant1",
			ls:   labels.NewBuilder(ls).Set("tenant", "tenant1").Labels(),
		},
		{
			user: "tenant2",
			ls:   labels.NewBuilder(ls).Set("tenant", "tenant2").Labels(),
		},
	}

	// add data for both tenants
	for _, tenant := range tenants {
		_ = h.Append(tenant.user, tenant.ls, labels.StableHash(tenant.ls), chks)

	}

	// ensure we're only returned the data from the correct tenant
	for _, tenant := range tenants {
		found, err := h.GetChunkRefs(
			context.Background(),
			tenant.user,
			0,
			100,
			nil, nil,
			labels.MustNewMatcher(labels.MatchEqual, "foo", "bar"),
		)
		require.Nil(t, err)
		require.Equal(t, chunkMetasToChunkRefs(tenant.user, labels.StableHash(tenant.ls), chks), found)
	}

}

// test head recover from wal
func Test_HeadManager_RecoverHead(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cases := []struct {
		Labels      labels.Labels
		Fingerprint uint64
		Chunks      []index.ChunkMeta
		User        string
	}{
		{
			User:        "tenant1",
			Labels:      mustParseLabels(`{foo="bar", bazz="buzz"}`),
			Fingerprint: labels.StableHash(mustParseLabels(`{foo="bar", bazz="buzz"}`)),
			Chunks: []index.ChunkMeta{
				{
					MinTime:  1,
					MaxTime:  10,
					Checksum: 3,
				},
			},
		},
		{
			User:        "tenant2",
			Labels:      mustParseLabels(`{foo="bard", bazz="bozz", bonk="borb"}`),
			Fingerprint: 1, // Different fingerprint should be preserved
			Chunks: []index.ChunkMeta{
				{
					MinTime:  1,
					MaxTime:  7,
					Checksum: 4,
				},
			},
		},
	}

	storeName := "store_2010-10-10"
	mgr := NewHeadManager(storeName, log.NewNopLogger(), dir, NewMetrics(nil), newNoopTSDBManager(storeName, dir))
	// This bit is normally handled by the Start() fn, but we're testing a smaller surface area
	// so ensure our dirs exist
	for _, d := range managerRequiredDirs(storeName, dir) {
		require.Nil(t, util.EnsureDirectory(d))
	}

	// Call Rotate() to ensure the new head tenant heads exist, etc
	require.Nil(t, mgr.Rotate(now))

	// now build a WAL independently to test recovery
	w, err := newHeadWAL(log.NewNopLogger(), walPath(mgr.name, mgr.dir, now), now)
	require.Nil(t, err)

	for i, c := range cases {
		require.Nil(t, w.Log(&WALRecord{
			UserID:      c.User,
			Fingerprint: c.Fingerprint,
			Series: record.RefSeries{
				Ref:    chunks.HeadSeriesRef(i),
				Labels: c.Labels,
			},
			Chks: ChunkMetasRecord{
				Chks: c.Chunks,
				Ref:  uint64(i),
			},
		}))
	}

	require.Nil(t, w.Stop())

	grp, ok, err := walsForPeriod(managerWalDir(mgr.name, mgr.dir), mgr.period, mgr.period.PeriodFor(now))
	require.Nil(t, err)
	require.True(t, ok)
	require.Equal(t, 1, len(grp.wals))
	require.Nil(t, recoverHead(mgr.name, mgr.dir, mgr.activeHeads, grp.wals, false, log.NewNopLogger(), NewMetrics(nil).walCorruptionsRepairs))

	for _, c := range cases {
		refs, err := mgr.GetChunkRefs(
			context.Background(),
			c.User,
			0, math.MaxInt64,
			nil, nil,
			labels.MustNewMatcher(labels.MatchRegexp, "foo", ".+"),
		)
		require.Nil(t, err)
		require.Equal(t, chunkMetasToChunkRefs(c.User, c.Fingerprint, c.Chunks), refs)
	}

}

// test head recover from corrupted wal
func Test_HeadManager_RecoverHead_CorruptedWAL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setupFunc func(t *testing.T, walPath string, w *headWAL)
		expectErr bool
	}{
		{
			name: "last record torn",
			setupFunc: func(t *testing.T, walPath string, w *headWAL) {
				// write enough records to fill a WAL page.
				for i := 0; i < 1000; i++ {
					require.Nil(t, w.Log(&WALRecord{
						UserID:      "tenant1",
						Fingerprint: labels.StableHash(mustParseLabels(`{foo="bar", bazz="buzz"}`)),
						Series: record.RefSeries{
							Ref:    chunks.HeadSeriesRef(i),
							Labels: mustParseLabels(`{foo="bar", bazz="buzz"}`),
						},
						Chks: ChunkMetasRecord{
							Chks: []index.ChunkMeta{
								{
									MinTime:  1,
									MaxTime:  10,
									Checksum: 3,
								},
							},
							Ref: uint64(i),
						},
					}))
				}

				require.Nil(t, w.Stop())

				// truncate the WAL file to 100 bytes.
				segmentFile := filepath.Join(walPath, "00000001")
				require.Nil(t, os.Truncate(segmentFile, 32*1024)) // 32kb
			},
		},
		{
			name: "invalid checksum",
			setupFunc: func(t *testing.T, walPath string, w *headWAL) {
				require.Nil(t, w.Log(&WALRecord{
					UserID:      "tenant1",
					Fingerprint: labels.StableHash(mustParseLabels(`{foo="bar", bazz="buzz"}`)),
					Series: record.RefSeries{
						Ref:    chunks.HeadSeriesRef(1),
						Labels: mustParseLabels(`{foo="bar", bazz="buzz"}`),
					},
					Chks: ChunkMetasRecord{
						Chks: []index.ChunkMeta{
							{
								MinTime:  1,
								MaxTime:  10,
								Checksum: 3,
							},
						},
						Ref: uint64(1),
					},
				}))
				require.Nil(t, w.Stop())
				// This will truncate the DATA part of the WAL record, causing the checksum to be invalid.
				require.Nil(t, os.Truncate(filepath.Join(walPath, "00000001"), 10)) // 7 bytes(header) + a little bit of data.
			},
		},
		{
			name: "invalid Loki WAL record",
			setupFunc: func(t *testing.T, _ string, w *headWAL) {
				require.Nil(t, w.wal.Log([]byte("not a valid Loki WAL record")))
				require.Nil(t, w.Stop())
			},
			// This is expected to fail because the WAL record is valid but the data part is not.
			// We cannot repair the wal in this case. This would only happen if we update the WALRecord format
			// and we try to replay a WAL that was written with the old format.
			expectErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()

			storeName := "store_2010-10-10"
			mgr := NewHeadManager(storeName, log.NewNopLogger(), dir, NewMetrics(nil), newNoopTSDBManager(storeName, dir))
			// This bit is normally handled by the Start() fn, but we're testing a smaller surface area
			// so ensure our dirs exist
			for _, d := range managerRequiredDirs(storeName, dir) {
				require.Nil(t, util.EnsureDirectory(d))
			}

			// Call Rotate() to ensure the new head tenant heads exist, etc
			require.Nil(t, mgr.Rotate(now))

			// now build a WAL independently to test recovery
			w, err := newHeadWAL(log.NewNopLogger(), walPath(mgr.name, mgr.dir, now), now)
			require.Nil(t, err)

			tc.setupFunc(t, walPath(mgr.name, mgr.dir, now), w)

			grp, ok, err := walsForPeriod(managerWalDir(mgr.name, mgr.dir), mgr.period, mgr.period.PeriodFor(now))
			require.Nil(t, err)
			require.True(t, ok)
			require.Equal(t, 1, len(grp.wals))

			err = recoverHead(mgr.name, mgr.dir, mgr.activeHeads, grp.wals, false, log.NewNopLogger(), NewMetrics(nil).walCorruptionsRepairs)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// test head still serves data for the most recently rotated period.
func Test_HeadManager_QueryAfterRotate(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cases := []struct {
		Labels      labels.Labels
		Fingerprint uint64
		Chunks      []index.ChunkMeta
		User        string
	}{
		{
			User:        "tenant1",
			Labels:      mustParseLabels(`{foo="bar", bazz="buzz"}`),
			Fingerprint: labels.StableHash(mustParseLabels(`{foo="bar", bazz="buzz"}`)),
			Chunks: []index.ChunkMeta{
				{
					MinTime:  1,
					MaxTime:  10,
					Checksum: 3,
				},
			},
		},
	}

	storeName := "store_2010-10-10"
	mgr := NewHeadManager(storeName, log.NewNopLogger(), dir, NewMetrics(nil), newNoopTSDBManager(storeName, dir))
	// This bit is normally handled by the Start() fn, but we're testing a smaller surface area
	// so ensure our dirs exist
	for _, d := range managerRequiredDirs(storeName, dir) {
		require.Nil(t, util.EnsureDirectory(d))
	}
	require.Nil(t, mgr.Rotate(now)) // initialize head (usually done by Start())

	// add data for both tenants
	for _, tc := range cases {
		require.Nil(t, mgr.Append(tc.User, tc.Labels, labels.StableHash(tc.Labels), tc.Chunks))
	}

	nextPeriod := time.Now().Add(time.Duration(mgr.period))
	mgr.tick(nextPeriod) // synthetic tick to rotate head

	for _, c := range cases {
		refs, err := mgr.GetChunkRefs(
			context.Background(),
			c.User,
			0, math.MaxInt64,
			nil, nil,
			labels.MustNewMatcher(labels.MatchRegexp, "foo", ".+"),
		)
		require.Nil(t, err)
		require.Equal(t, chunkMetasToChunkRefs(c.User, c.Fingerprint, c.Chunks), refs)
	}

}

// test mgr recover from multiple wals across multiple periods
func Test_HeadManager_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	curPeriod := time.Now()
	cases := []struct {
		Labels labels.Labels
		Chunks []index.ChunkMeta
		User   string
	}{
		{
			User:   "tenant1",
			Labels: mustParseLabels(`{foo="bar", bazz="buzz"}`),
			Chunks: []index.ChunkMeta{
				{
					MinTime:  1,
					MaxTime:  10,
					Checksum: 3,
				},
			},
		},
		{
			User:   "tenant2",
			Labels: mustParseLabels(`{foo="bard", bazz="bozz", bonk="borb"}`),
			Chunks: []index.ChunkMeta{
				{
					MinTime:  1,
					MaxTime:  7,
					Checksum: 4,
				},
			},
		},
	}

	storeName := "store_2010-10-10"
	mgr := NewHeadManager(storeName, log.NewNopLogger(), dir, NewMetrics(nil), newNoopTSDBManager(storeName, dir))
	w, err := newHeadWAL(log.NewNopLogger(), walPath(mgr.name, mgr.dir, curPeriod), curPeriod)
	require.Nil(t, err)

	// Write old WALs
	for i, c := range cases {
		require.Nil(t, w.Log(&WALRecord{
			UserID:      c.User,
			Fingerprint: labels.StableHash(c.Labels),
			Series: record.RefSeries{
				Ref:    chunks.HeadSeriesRef(i),
				Labels: c.Labels,
			},
			Chks: ChunkMetasRecord{
				Chks: c.Chunks,
				Ref:  uint64(i),
			},
		}))
	}

	require.Nil(t, w.Stop())

	// Start, ensuring recovery from old WALs
	require.Nil(t, mgr.Start())

	// Ensure old WAL data is queryable
	multiIndex := NewMultiIndex(IndexSlice{mgr, mgr.tsdbManager.(noopTSDBManager).tenantHeads})

	for _, c := range cases {
		refs, err := multiIndex.GetChunkRefs(
			context.Background(),
			c.User,
			0, math.MaxInt64,
			nil, nil,
			labels.MustNewMatcher(labels.MatchRegexp, "foo", ".+"),
		)
		require.Nil(t, err)

		lbls := labels.NewBuilder(c.Labels)
		lbls.Set(TenantLabel, c.User)
		require.Equal(t, chunkMetasToChunkRefs(c.User, labels.StableHash(c.Labels), c.Chunks), refs)
	}

	// Add data
	newCase := struct {
		Labels labels.Labels
		Chunks []index.ChunkMeta
		User   string
	}{
		User:   "tenant3",
		Labels: mustParseLabels(`{foo="bard", other="hi"}`),
		Chunks: []index.ChunkMeta{
			{
				MinTime:  1,
				MaxTime:  7,
				Checksum: 4,
			},
		},
	}

	require.Nil(t, mgr.Append(newCase.User, newCase.Labels, labels.StableHash(newCase.Labels), newCase.Chunks))

	// Ensure old + new data is queryable
	for _, c := range append(cases, newCase) {
		refs, err := multiIndex.GetChunkRefs(
			context.Background(),
			c.User,
			0, math.MaxInt64,
			nil, nil,
			labels.MustNewMatcher(labels.MatchRegexp, "foo", ".+"),
		)
		require.Nil(t, err)

		lbls := labels.NewBuilder(c.Labels)
		lbls.Set(TenantLabel, c.User)
		require.Equal(t, chunkMetasToChunkRefs(c.User, labels.StableHash(c.Labels), c.Chunks), refs)
	}
}

func TestBuildLegacyWALs(t *testing.T) {
	dir := t.TempDir()

	secondStoreDate := parseDate("2023-01-02")
	schemaCfgs := []config.SchemaConfig{
		{
			Configs: []config.PeriodConfig{
				{
					Schema:     "v11",
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
				{
					Schema:     "v11",
					From:       config.DayTime{Time: timeToModelTime(secondStoreDate)},
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
			},
		}, {
			Configs: []config.PeriodConfig{
				{
					Schema:     "v12",
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
				{
					Schema:     "v12",
					From:       config.DayTime{Time: timeToModelTime(secondStoreDate)},
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
			},
		}, {
			Configs: []config.PeriodConfig{
				{
					Schema:     "v13",
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
				{
					Schema:     "v13",
					From:       config.DayTime{Time: timeToModelTime(secondStoreDate)},
					IndexType:  types.TSDBType,
					ObjectType: types.StorageTypeFileSystem,
					IndexTables: config.IndexPeriodicTableConfig{
						PeriodicTableConfig: config.PeriodicTableConfig{
							Prefix: "index_",
							Period: time.Hour * 24,
						}},
				},
			},
		},
	}

	c := struct {
		Labels      labels.Labels
		Fingerprint uint64
		Chunks      []index.ChunkMeta
		User        string
	}{
		User:        "tenant1",
		Labels:      mustParseLabels(`{foo="bar", bazz="buzz"}`),
		Fingerprint: labels.StableHash(mustParseLabels(`{foo="bar", bazz="buzz"}`)),
		Chunks: []index.ChunkMeta{
			{
				MinTime:  secondStoreDate.Add(-36 * time.Hour).UnixMilli(),
				MaxTime:  secondStoreDate.Add(-24 * time.Hour).UnixMilli(),
				Checksum: 3,
			},
			// chunk overlapping the period boundary
			{
				MinTime:  secondStoreDate.Add(-1 * time.Hour).UnixMilli(),
				MaxTime:  secondStoreDate.Add(1 * time.Hour).UnixMilli(),
				Checksum: 3,
			},
			{
				MinTime:  secondStoreDate.Add(24 * time.Hour).UnixMilli(),
				MaxTime:  secondStoreDate.Add(36 * time.Hour).UnixMilli(),
				Checksum: 3,
			},
		},
	}

	// populate WAL file with chunks from two different periods
	now := time.Now()
	w, err := newHeadWAL(log.NewNopLogger(), legacyWalPath(dir, now), now)
	require.Nil(t, err)
	require.Nil(t, w.Log(&WALRecord{
		UserID:      c.User,
		Fingerprint: c.Fingerprint,
		Series: record.RefSeries{
			Ref:    chunks.HeadSeriesRef(123),
			Labels: c.Labels,
		},
		Chks: ChunkMetasRecord{
			Chks: c.Chunks,
			Ref:  uint64(123),
		},
	}))
	require.Nil(t, w.Stop())

	fsObjectClient, err := local.NewFSObjectClient(local.FSConfig{Directory: filepath.Join(dir, "fs_store")})
	require.Nil(t, err)

	shipperCfg := indexshipper.Config{}
	flagext.DefaultValues(&shipperCfg)
	shipperCfg.Mode = indexshipper.ModeReadWrite
	shipperCfg.ActiveIndexDirectory = filepath.Join(dir)
	shipperCfg.CacheLocation = filepath.Join(dir, "cache")

	for _, schema := range schemaCfgs {
		schemaCfg := schema
		for _, tc := range []struct {
			name, store    string
			tableRange     config.TableRange
			expectedChunks []logproto.ChunkRef
		}{
			{
				name:           "query-period-1",
				store:          "period-1",
				tableRange:     schemaCfg.Configs[0].GetIndexTableNumberRange(config.DayTime{Time: timeToModelTime(secondStoreDate.Add(-time.Millisecond))}),
				expectedChunks: chunkMetasToLogProtoChunkRefs(c.User, labels.StableHash(c.Labels), c.Chunks[:2]),
			},
			{
				name:           "query-period-2",
				store:          "period-2",
				tableRange:     schemaCfg.Configs[1].GetIndexTableNumberRange(config.DayTime{Time: math.MaxInt64}),
				expectedChunks: chunkMetasToLogProtoChunkRefs(c.User, labels.StableHash(c.Labels), c.Chunks[1:]),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store, stop, err := NewStore(tc.store, "index/", shipperCfg, schemaCfg, nil, fsObjectClient, &zeroValueLimits{}, tc.tableRange, nil, log.NewNopLogger())
				require.Nil(t, err)
				refs, err := store.GetChunkRefs(
					context.Background(),
					c.User,
					0, timeToModelTime(secondStoreDate.Add(48*time.Hour)),
					chunk.NewPredicate([]*labels.Matcher{labels.MustNewMatcher(labels.MatchRegexp, "foo", ".+")}, nil),
				)
				require.Nil(t, err)
				require.Equal(t, tc.expectedChunks, refs)

				stop()
			})
		}
	}
}

func parseDate(in string) time.Time {
	t, err := time.Parse("2006-01-02", in)
	if err != nil {
		panic(err)
	}
	return t
}

func timeToModelTime(t time.Time) model.Time {
	return model.TimeFromUnixNano(t.UnixNano())
}

func BenchmarkTenantHeads(b *testing.B) {
	for _, tc := range []struct {
		readers, writers int
	}{
		{
			readers: 10,
		},
		{
			readers: 100,
		},
		{
			readers: 1000,
		},
	} {
		b.Run(fmt.Sprintf("%d", tc.readers), func(b *testing.B) {
			heads := newTenantHeads(time.Now(), defaultHeadManagerStripeSize, NewMetrics(nil), log.NewNopLogger())
			// 1000 series across 100 tenants
			nTenants := 10
			for i := 0; i < 1000; i++ {
				tenant := i % nTenants
				ls := mustParseLabels(fmt.Sprintf(`{foo="bar", i="%d"}`, i))
				heads.Append(fmt.Sprint(tenant), ls, labels.StableHash(ls), index.ChunkMetas{
					{},
				})
			}

			for n := 0; n < b.N; n++ {
				var wg sync.WaitGroup
				for r := 0; r < tc.readers; r++ {
					wg.Add(1)
					go func(r int) {
						defer wg.Done()
						var res []ChunkRef
						tenant := r % nTenants

						// nolint:ineffassign,staticcheck
						res, _ = heads.GetChunkRefs(
							context.Background(),
							fmt.Sprint(tenant),
							0, math.MaxInt64,
							res,
							nil,
							labels.MustNewMatcher(labels.MatchEqual, "foo", "bar"),
						)
					}(r)
				}

				wg.Wait()
			}

		})
	}
}
