package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/planetlabs/go-stac/stats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

const (
	testExtensionV020 = "https://stac-extensions.github.io/stats/v0.2.0/schema.json"
	testExtensionV100 = "https://stac-extensions.github.io/stats/v1.0.0/schema.json"
)

var testCatalogEntry = filepath.Join("..", "..", "crawler", "testdata", "v1.0.0", "catalog-with-collection-of-items.json")

func runStatsCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	app := &cli.App{
		Name:      "stac",
		Commands:  []*cli.Command{statsCommand},
		Writer:    out,
		ErrWriter: io.Discard,
	}
	err := app.Run(append([]string{"stac", "stats"}, args...))
	return out.String(), err
}

func writeShardFile(t *testing.T, dir, name, extensionURI string, summary *stats.Stats) string {
	t.Helper()
	shard, err := stats.NewShard(extensionURI)
	require.NoError(t, err)
	shard.Stats = summary
	path := filepath.Join(dir, name)
	require.NoError(t, stats.WriteShard(path, shard))
	return path
}

func shardCatalogs(count uint64, versions map[string]uint64) *stats.Stats {
	return &stats.Stats{
		Catalogs: &stats.ResourceStats{Count: count, Versions: versions},
	}
}

func TestStatsCrawlStdoutPath(t *testing.T) {
	// exercises the existing crawl -> stdout statistics path (no network)
	output, err := runStatsCommand(t, "--entry", testCatalogEntry)
	require.NoError(t, err)

	summary := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal([]byte(output), &summary))
	assert.Contains(t, summary, "stats:catalogs")
	assert.Contains(t, summary, "stats:collections")
	assert.Contains(t, summary, "stats:items")
	assert.NotContains(t, summary, "version")

	catalogs := map[string]any{}
	require.NoError(t, json.Unmarshal(summary["stats:catalogs"], &catalogs))
	assert.EqualValues(t, 1, catalogs["count"])
	items := map[string]any{}
	require.NoError(t, json.Unmarshal(summary["stats:items"], &items))
	assert.EqualValues(t, 1, items["count"])
	assets := items["assets"].(map[string]any)
	assert.EqualValues(t, 1, assets["image/jpeg"])
	assert.EqualValues(t, 1, assets["image/tiff; application=geotiff; profile=cloud-optimized"])
	assert.EqualValues(t, 1, assets[""])
}

func TestStatsOfflineOutput(t *testing.T) {
	// any accidental network access would try to use this unreachable proxy
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")

	originalBytes, err := os.ReadFile(testCatalogEntry)
	require.NoError(t, err)

	outputPath := filepath.Join(t.TempDir(), "catalog-with-stats.json")
	_, err = runStatsCommand(t,
		"--entry", testCatalogEntry,
		"--output", outputPath,
		"--stats-extension", testExtensionV020,
	)
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	resource := map[string]any{}
	require.NoError(t, json.Unmarshal(data, &resource))

	extensions := resource["stac_extensions"].([]any)
	require.Len(t, extensions, 1)
	assert.Equal(t, testExtensionV020, extensions[0])

	// the entry itself is excluded from the child statistics
	assert.NotContains(t, resource, "stats:catalogs")
	assert.Contains(t, resource, "stats:collections")
	assert.Contains(t, resource, "stats:items")

	// the original entry resource is not modified
	currentBytes, err := os.ReadFile(testCatalogEntry)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalBytes, currentBytes))
}

func TestStatsOfflineOutputInvalidExtension(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.json")
	_, err := runStatsCommand(t,
		"--entry", testCatalogEntry,
		"--output", outputPath,
		"--stats-extension", "not-a-uri",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid stats extension URI")
	_, statErr := os.Stat(outputPath)
	assert.True(t, os.IsNotExist(statErr))
}

func TestStatsShardOutput(t *testing.T) {
	dir := t.TempDir()
	shardPath := filepath.Join(dir, "shard.json")
	_, err := runStatsCommand(t,
		"--entry", testCatalogEntry,
		"--shard-output", shardPath,
		"--stats-extension", testExtensionV020,
	)
	require.NoError(t, err)

	shard, err := stats.ReadShard(shardPath)
	require.NoError(t, err)
	assert.Equal(t, testExtensionV020, shard.Extension)
	assert.Equal(t, uint64(1), shard.Stats.Count(stats.Catalog))
	assert.Equal(t, uint64(1), shard.Stats.Count(stats.Collection))
	assert.Equal(t, uint64(1), shard.Stats.Count(stats.Item))
}

func TestStatsShardOutputRequiresExtension(t *testing.T) {
	shardPath := filepath.Join(t.TempDir(), "shard.json")
	_, err := runStatsCommand(t, "--entry", testCatalogEntry, "--shard-output", shardPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--"+flagStatsExtension)
}

func TestStatsMergeShardsStdoutOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	first := writeShardFile(t, dir, "first.json", testExtensionV020, &stats.Stats{
		Catalogs: &stats.ResourceStats{
			Count:    2,
			Versions: map[string]uint64{"1.0.0": 2},
		},
		Items: &stats.ResourceStats{
			Count:    3,
			Versions: map[string]uint64{"1.0.0": 3},
			Assets:   map[string]uint64{"image/png": 3},
		},
	})
	second := writeShardFile(t, dir, "second.json", testExtensionV020, &stats.Stats{
		Catalogs: &stats.ResourceStats{
			Count:    1,
			Versions: map[string]uint64{"0.9.0": 1},
		},
		Items: &stats.ResourceStats{
			Count:    2,
			Versions: map[string]uint64{"1.0.0": 2},
			Assets:   map[string]uint64{"image/png": 1, "image/jpeg": 1},
		},
	})

	forward, err := runStatsCommand(t, "--shard", first, "--shard", second)
	require.NoError(t, err)
	reverse, err := runStatsCommand(t, "--shard", second, "--shard", first)
	require.NoError(t, err)
	assert.JSONEq(t, forward, reverse)
	assert.Equal(t, forward, reverse)

	summary := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal([]byte(forward), &summary))
	catalogs := map[string]any{}
	require.NoError(t, json.Unmarshal(summary["stats:catalogs"], &catalogs))
	assert.EqualValues(t, 3, catalogs["count"])
	items := map[string]any{}
	require.NoError(t, json.Unmarshal(summary["stats:items"], &items))
	assert.EqualValues(t, 5, items["count"])
	assets := items["assets"].(map[string]any)
	assert.EqualValues(t, 4, assets["image/png"])
	assert.EqualValues(t, 1, assets["image/jpeg"])
}

func TestStatsMergeShardsToOutput(t *testing.T) {
	dir := t.TempDir()
	shard := writeShardFile(t, dir, "shard.json", testExtensionV020, shardCatalogs(2, map[string]uint64{"1.0.0": 2}))

	entryPath := filepath.Join(dir, "catalog.json")
	require.NoError(t, os.WriteFile(entryPath, []byte(`{
  "stac_version": "1.0.0",
  "type": "Catalog",
  "id": "root",
  "description": "entry",
  "links": []
}`), 0644))
	outputPath := filepath.Join(dir, "catalog-with-stats.json")

	_, err := runStatsCommand(t,
		"--shard", shard,
		"--entry", entryPath,
		"--output", outputPath,
	)
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	resource := map[string]any{}
	require.NoError(t, json.Unmarshal(data, &resource))
	extensions := resource["stac_extensions"].([]any)
	assert.Equal(t, testExtensionV020, extensions[0])
	catalogs := resource["stats:catalogs"].(map[string]any)
	assert.EqualValues(t, 2, catalogs["count"])

	entryBytes, err := os.ReadFile(entryPath)
	require.NoError(t, err)
	assert.Contains(t, string(entryBytes), "\"id\": \"root\"")
	assert.NotContains(t, string(entryBytes), "stats:catalogs")
}

func TestStatsMergeMergedShardOutput(t *testing.T) {
	dir := t.TempDir()
	first := writeShardFile(t, dir, "first.json", testExtensionV020, shardCatalogs(2, map[string]uint64{"1.0.0": 2}))
	second := writeShardFile(t, dir, "second.json", testExtensionV020, shardCatalogs(3, map[string]uint64{"1.0.0": 3}))
	mergedPath := filepath.Join(dir, "merged.json")

	_, err := runStatsCommand(t, "--shard", first, "--shard", second, "--shard-output", mergedPath)
	require.NoError(t, err)

	merged, err := stats.ReadShard(mergedPath)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), merged.Stats.Count(stats.Catalog))
	assert.Equal(t, uint64(5), merged.Stats.Catalogs.Versions["1.0.0"])
}

func TestStatsMergeVersionConflict(t *testing.T) {
	dir := t.TempDir()
	valid := writeShardFile(t, dir, "valid.json", testExtensionV020, shardCatalogs(1, map[string]uint64{"1.0.0": 1}))
	future := filepath.Join(dir, "future.json")
	require.NoError(t, os.WriteFile(future, []byte(
		`{"version":2,"extension":"`+testExtensionV020+`","stats":{"stats:catalogs":{"count":1,"versions":{"1.0.0":1}}}}`,
	), 0644))

	_, err := runStatsCommand(t, "--shard", valid, "--shard", future)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported shard version 2")
}

func TestStatsMergeExtensionConflict(t *testing.T) {
	dir := t.TempDir()
	first := writeShardFile(t, dir, "first.json", testExtensionV020, shardCatalogs(1, map[string]uint64{"1.0.0": 1}))
	second := writeShardFile(t, dir, "second.json", testExtensionV100, shardCatalogs(1, map[string]uint64{"1.0.0": 1}))

	_, err := runStatsCommand(t, "--shard", first, "--shard", second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stats extension conflict")
}

func TestStatsMergeFlagExtensionMismatch(t *testing.T) {
	dir := t.TempDir()
	shard := writeShardFile(t, dir, "shard.json", testExtensionV020, shardCatalogs(1, map[string]uint64{"1.0.0": 1}))

	_, err := runStatsCommand(t, "--shard", shard, "--stats-extension", testExtensionV100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stats extension conflict")
}

func TestStatsMergeMalformedShard(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")
	require.NoError(t, os.WriteFile(malformed, []byte(`{"version":1,"stats":{}}`), 0644))

	_, err := runStatsCommand(t, "--shard", malformed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid stats extension URI")
}

func TestStatsMergeEntryWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	shard := writeShardFile(t, dir, "shard.json", testExtensionV020, shardCatalogs(1, map[string]uint64{"1.0.0": 1}))
	entry := filepath.Join(dir, "catalog.json")
	require.NoError(t, os.WriteFile(entry, []byte(`{"stac_version":"1.0.0","type":"Catalog","id":"root","links":[]}`), 0644))

	_, err := runStatsCommand(t, "--shard", shard, "--entry", entry)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--"+flagOutput)
}

func TestStatsMissingEntry(t *testing.T) {
	_, err := runStatsCommand(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing --"+flagEntry)
}
