package stats

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/planetlabs/go-stac/crawler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	extensionV020 = "https://stac-extensions.github.io/stats/v0.2.0/schema.json"
	extensionV100 = "https://stac-extensions.github.io/stats/v1.0.0/schema.json"
)

func shardWith(t *testing.T, extensionURI string, summary *Stats) *Shard {
	t.Helper()
	shard, err := NewShard(extensionURI)
	require.NoError(t, err)
	shard.Stats = summary
	require.NoError(t, shard.Validate())
	return shard
}

func TestExtensionVersion(t *testing.T) {
	cases := []struct {
		uri     string
		version string
	}{
		{extensionV020, "v0.2.0"},
		{extensionV100, "v1.0.0"},
		{"https://stac-extensions.github.io/stats/v1.0.0-rc.1/schema.json", "v1.0.0-rc.1"},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			version, err := ExtensionVersion(c.uri)
			require.NoError(t, err)
			assert.Equal(t, c.version, version)
		})
	}

	invalid := []string{
		"",
		"garbage",
		"http://stac-extensions.github.io/stats/v1.0.0/schema.json",
		"https://example.com/stats/v1.0.0/schema.json",
		"https://stac-extensions.github.io/stats/v1/schema.json",
		"https://stac-extensions.github.io/stats/v1.0.0/schema",
		"https://stac-extensions.github.io/stats/v1.0.0/",
		"https://stac-extensions.github.io/other/v1.0.0/schema.json",
	}
	for _, uri := range invalid {
		t.Run("invalid/"+uri, func(t *testing.T) {
			_, err := ExtensionVersion(uri)
			assert.Error(t, err)
		})
	}
}

func TestNewShardRejectsBadURI(t *testing.T) {
	_, err := NewShard("not-a-uri")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid stats extension URI")
}

func TestMergeSingleShard(t *testing.T) {
	summary := &Stats{
		Catalogs: &ResourceStats{
			Count:    2,
			Versions: map[string]uint64{"1.0.0": 2},
		},
		Items: &ResourceStats{
			Count:      4,
			Versions:   map[string]uint64{"1.0.0": 4},
			Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 2},
			Assets:     map[string]uint64{"image/png": 4},
		},
	}
	shard := shardWith(t, extensionV020, summary)

	merged, err := Merge(shard)
	require.NoError(t, err)
	assert.Equal(t, ShardVersion, merged.Version)
	assert.Equal(t, extensionV020, merged.Extension)
	assert.Equal(t, uint64(2), merged.Stats.Count(Catalog))
	assert.Equal(t, uint64(0), merged.Stats.Count(Collection))
	assert.Equal(t, uint64(4), merged.Stats.Count(Item))
	assert.Nil(t, merged.Stats.Collections)
	assert.Equal(t, uint64(2), merged.Stats.Items.Extensions["https://stac-extensions.github.io/eo/v1.0.0/schema.json"])
}

func TestMergeDuplicateCounts(t *testing.T) {
	first := shardWith(t, extensionV020, &Stats{
		Catalogs: &ResourceStats{
			Count:    3,
			Versions: map[string]uint64{"1.0.0": 2, "1.0.0-rc.4": 1},
		},
	})
	second := shardWith(t, extensionV020, &Stats{
		Catalogs: &ResourceStats{
			Count:    5,
			Versions: map[string]uint64{"1.0.0": 5},
		},
	})

	merged, err := Merge(first, second)
	require.NoError(t, err)
	require.NotNil(t, merged.Stats.Catalogs)
	assert.Equal(t, uint64(8), merged.Stats.Catalogs.Count)
	assert.Equal(t, uint64(7), merged.Stats.Catalogs.Versions["1.0.0"])
	assert.Equal(t, uint64(1), merged.Stats.Catalogs.Versions["1.0.0-rc.4"])
}

func TestMergeOrderIndependent(t *testing.T) {
	shards := []*Shard{
		shardWith(t, extensionV020, &Stats{
			Catalogs: &ResourceStats{
				Count:       2,
				Versions:    map[string]uint64{"1.0.0": 2},
				Conformance: map[string]uint64{"http://example.com/conf/a": 1},
			},
			Collections: &ResourceStats{
				Count:      1,
				Versions:   map[string]uint64{"1.0.0": 1},
				Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 1},
			},
		}),
		shardWith(t, extensionV020, &Stats{
			Catalogs: &ResourceStats{
				Count:       3,
				Versions:    map[string]uint64{"1.0.0": 2, "0.9.0": 1},
				Conformance: map[string]uint64{"http://example.com/conf/a": 2, "http://example.com/conf/b": 1},
			},
			Items: &ResourceStats{
				Count:    4,
				Versions: map[string]uint64{"1.0.0": 4},
				Assets:   map[string]uint64{"image/png": 3, "image/jpeg": 1},
			},
		}),
		shardWith(t, extensionV020, &Stats{
			Collections: &ResourceStats{
				Count:      2,
				Versions:   map[string]uint64{"1.0.0": 1, "0.9.0": 1},
				Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 1},
			},
			Items: &ResourceStats{
				Count:      1,
				Versions:   map[string]uint64{"1.0.0": 1},
				Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 1},
				Assets:     map[string]uint64{"image/png": 1},
			},
		}),
	}

	expected := &Stats{
		Catalogs: &ResourceStats{
			Count:       5,
			Versions:    map[string]uint64{"1.0.0": 4, "0.9.0": 1},
			Conformance: map[string]uint64{"http://example.com/conf/a": 3, "http://example.com/conf/b": 1},
		},
		Collections: &ResourceStats{
			Count:      3,
			Versions:   map[string]uint64{"1.0.0": 2, "0.9.0": 1},
			Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 2},
		},
		Items: &ResourceStats{
			Count:      5,
			Versions:   map[string]uint64{"1.0.0": 5},
			Extensions: map[string]uint64{"https://stac-extensions.github.io/eo/v1.0.0/schema.json": 1},
			Assets:     map[string]uint64{"image/png": 4, "image/jpeg": 1},
		},
	}

	permutations := [][]int{
		{0, 1, 2},
		{0, 2, 1},
		{1, 0, 2},
		{1, 2, 0},
		{2, 0, 1},
		{2, 1, 0},
	}

	var reference []byte
	for i, order := range permutations {
		ordered := make([]*Shard, len(order))
		for j, index := range order {
			ordered[j] = shards[index]
		}
		merged, err := Merge(ordered...)
		require.NoError(t, err)
		if !assert.Equal(t, expected, merged.Stats, "permutation %v", order) {
			return
		}
		data, jsonErr := json.Marshal(merged)
		require.NoError(t, jsonErr)
		if i == 0 {
			reference = data
		} else {
			assert.True(t, bytes.Equal(reference, data), "serialized merge differs for permutation %v", order)
		}
	}
}

func TestMergeRequiresShards(t *testing.T) {
	_, err := Merge()
	require.Error(t, err)
}

func TestMergeExtensionConflict(t *testing.T) {
	first := shardWith(t, extensionV020, &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	})
	second := shardWith(t, extensionV100, &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	})

	_, err := Merge(first, second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stats extension conflict")
	assert.Contains(t, err.Error(), extensionV020)
	assert.Contains(t, err.Error(), extensionV100)
}

func TestMergeUnknownVersion(t *testing.T) {
	summary := &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	}
	oldShard := shardWith(t, extensionV020, summary)

	futureShard := &Shard{Version: 2, Extension: extensionV020, Stats: &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	}}
	_, err := Merge(oldShard, futureShard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported shard version 2")

	missingVersion := &Shard{Extension: extensionV020, Stats: &Stats{}}
	err = missingVersion.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing shard version")
}

func TestLoadShardValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
		err  string
	}{
		{
			name: "unknown version",
			json: `{"version":99,"extension":"` + extensionV020 + `","stats":{}}`,
			err:  "unsupported shard version 99",
		},
		{
			name: "missing version",
			json: `{"extension":"` + extensionV020 + `","stats":{}}`,
			err:  "missing shard version",
		},
		{
			name: "bad extension uri",
			json: `{"version":1,"extension":"https://example.com/schema.json","stats":{}}`,
			err:  "invalid stats extension URI",
		},
		{
			name: "missing stats",
			json: `{"version":1,"extension":"` + extensionV020 + `"}`,
			err:  "missing the stats object",
		},
		{
			name: "unknown top level field",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{},"extra":1}`,
			err:  "unknown field",
		},
		{
			name: "unknown stats field",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:catalogs":{"count":1,"versions":{"1.0.0":1},"bogus":{}}}}`,
			err:  "unknown field",
		},
		{
			name: "zero catalog count",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:catalogs":{"count":0,"versions":{}}}}`,
			err:  "zero count",
		},
		{
			name: "zero version count",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:catalogs":{"count":1,"versions":{"1.0.0":0}}}}`,
			err:  "zero versions count",
		},
		{
			name: "versions do not add up",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:catalogs":{"count":2,"versions":{"1.0.0":1}}}}`,
			err:  "do not add up",
		},
		{
			name: "extensions exceed count",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:collections":{"count":1,"versions":{"1.0.0":1},"extensions":{"http://x":2}}}}`,
			err:  "exceed the resource count",
		},
		{
			name: "conformance on collection",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:collections":{"count":1,"versions":{"1.0.0":1},"conformance":{"http://x":1}}}}`,
			err:  "not valid for collection stats",
		},
		{
			name: "assets on catalog",
			json: `{"version":1,"extension":"` + extensionV020 + `","stats":{"stats:catalogs":{"count":1,"versions":{"1.0.0":1},"assets":{"image/png":1}}}}`,
			err:  "not valid for catalog stats",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadShard(strings.NewReader(c.json))
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.err)
		})
	}
}

func TestShardRoundTrip(t *testing.T) {
	shard := shardWith(t, extensionV020, &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	})

	data, err := shard.Marshal()
	require.NoError(t, err)

	loaded, err := LoadShard(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, shard.Version, loaded.Version)
	assert.Equal(t, shard.Extension, loaded.Extension)
	assert.Equal(t, shard.Stats, loaded.Stats)
}

func TestShardFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shard.json")
	shard := shardWith(t, extensionV020, &Stats{
		Items: &ResourceStats{
			Count:    1,
			Versions: map[string]uint64{"1.0.0": 1},
			Assets:   map[string]uint64{"image/png": 1},
		},
	})

	require.NoError(t, WriteShard(path, shard))

	loaded, err := ReadShard(path)
	require.NoError(t, err)
	assert.Equal(t, shard.Stats, loaded.Stats)

	_, err = ReadShard(filepath.Join(dir, "missing.json"))
	assert.Error(t, err)
}

func TestEmptyCollectorOmitsZeroCounts(t *testing.T) {
	collector, err := NewCollector(extensionV020)
	require.NoError(t, err)

	data, err := json.Marshal(collector.Snapshot())
	require.NoError(t, err)
	assert.Equal(t, "{}", string(data))

	shard, err := collector.Shard()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), shard.Stats.Count(Catalog))
	assert.Nil(t, shard.Stats.Catalogs)
}

func TestCollectorShardRequiresExtension(t *testing.T) {
	collector, err := NewCollector("")
	require.NoError(t, err)
	_, err = collector.Shard()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without a stats extension URI")

	_, err = NewCollector("bad uri")
	require.Error(t, err)
}

func TestCollectorCrawl(t *testing.T) {
	entryPath := filepath.Join("..", "crawler", "testdata", "v1.0.0", "catalog-with-collection-of-items.json")

	collector, err := NewCollector(extensionV020)
	require.NoError(t, err)

	require.NoError(t, crawler.Crawl(entryPath, collector.Visit))

	summary := collector.Snapshot()
	assert.Equal(t, uint64(1), summary.Count(Catalog))
	assert.Equal(t, uint64(1), summary.Count(Collection))
	assert.Equal(t, uint64(1), summary.Count(Item))

	require.NotNil(t, summary.Catalogs)
	assert.Equal(t, map[string]uint64{"1.0.0": 1}, summary.Catalogs.Versions)
	require.NotNil(t, summary.Items)
	assert.Equal(t, map[string]uint64{"1.0.0": 1}, summary.Items.Versions)
	assert.Equal(t, map[string]uint64{
		"":           1,
		"image/jpeg": 1,
		"image/tiff; application=geotiff; profile=cloud-optimized": 1,
	}, summary.Items.Assets)

	shard, err := collector.Shard()
	require.NoError(t, err)
	require.NoError(t, shard.Validate())

	data, marshalErr := shard.Marshal()
	require.NoError(t, marshalErr)
	loaded, loadErr := LoadShard(bytes.NewReader(data))
	require.NoError(t, loadErr)
	assert.True(t, reflect.DeepEqual(shard.Stats, loaded.Stats))
}

func TestCollectorConformanceAndExtensions(t *testing.T) {
	collector, err := NewCollector(extensionV020)
	require.NoError(t, err)

	conformanceURI := "http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/core"
	extensionURI := "https://stac-extensions.github.io/eo/v1.0.0/schema.json"
	resource := crawler.Resource{
		"type":            "Catalog",
		"stac_version":    "1.0.0",
		"conformsTo":      []interface{}{conformanceURI},
		"stac_extensions": []interface{}{extensionURI},
	}
	collector.Count(resource)
	collector.Count(resource)

	summary := collector.Snapshot()
	require.NotNil(t, summary.Catalogs)
	assert.Equal(t, uint64(2), summary.Catalogs.Count)
	assert.Equal(t, uint64(2), summary.Catalogs.Conformance[conformanceURI])
	assert.Equal(t, uint64(2), summary.Catalogs.Extensions[extensionURI])
	assert.Nil(t, summary.Catalogs.Assets)
}

func TestCollectorSnapshotIsIndependent(t *testing.T) {
	collector, err := NewCollector(extensionV020)
	require.NoError(t, err)
	collector.Count(crawler.Resource{"type": "Catalog", "stac_version": "1.0.0"})

	snapshot := collector.Snapshot()
	snapshot.Catalogs.Count = 99
	snapshot.Catalogs.Versions["1.0.0"] = 99

	fresh := collector.Snapshot()
	assert.Equal(t, uint64(1), fresh.Catalogs.Count)
	assert.Equal(t, uint64(1), fresh.Catalogs.Versions["1.0.0"])
}

func TestAddIsOrderIndependent(t *testing.T) {
	left := &Stats{}
	right := &Stats{
		Catalogs: &ResourceStats{Count: 1, Versions: map[string]uint64{"1.0.0": 1}},
	}
	left.Add(right)
	left.Add(nil)
	assert.Equal(t, uint64(1), left.Count(Catalog))
}
