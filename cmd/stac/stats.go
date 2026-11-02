package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v51/github"
	"github.com/planetlabs/go-stac/crawler"
	"github.com/planetlabs/go-stac/stats"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

const (
	statsRepoOwner = "stac-extensions"
	statsRepoName  = "stats"
)

var statsCommand = &cli.Command{
	Name:        "stats",
	Usage:       "Generate STAC statistics",
	Description: "Crawls STAC resources and reports on statistics. Statistics for different partitions can be written as versioned shards and later merged without accessing the network.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    flagEntry,
			Usage:   "Path or URL to STAC resource (catalog, collection, or item) to crawl",
			EnvVars: []string{toEnvVar(flagEntry)},
		},
		&cli.StringFlag{
			Name:    flagOutput,
			Usage:   "Path to write a copy of the entry resource with statistics added (if not provided, stats will be written to stdout)",
			EnvVars: []string{toEnvVar(flagOutput)},
		},
		&cli.StringFlag{
			Name: flagStatsExtension,
			Usage: "URI of the STAC Stats extension schema to use (e.g. https://stac-extensions.github.io/stats/v0.2.0/schema.json); " +
				"when provided, statistics are written without accessing the network",
			EnvVars: []string{toEnvVar(flagStatsExtension)},
		},
		&cli.StringSliceFlag{
			Name:    flagShard,
			Usage:   "Path to a versioned stats shard from a partial crawl; may be provided multiple times to merge shards (no crawl is performed)",
			EnvVars: []string{toEnvVar(flagShard)},
		},
		&cli.StringFlag{
			Name:    flagShardOutput,
			Usage:   "Path to write the collected statistics as a versioned stats shard (requires --" + flagStatsExtension + ")",
			EnvVars: []string{toEnvVar(flagShardOutput)},
		},
	},
	Action: func(ctx *cli.Context) error {
		entryPath := ctx.String(flagEntry)
		outputPath := ctx.String(flagOutput)
		shardOutputPath := ctx.String(flagShardOutput)
		extensionURI := ctx.String(flagStatsExtension)
		shardPaths := ctx.StringSlice(flagShard)

		if extensionURI != "" {
			if _, err := stats.ExtensionVersion(extensionURI); err != nil {
				return err
			}
		}

		if len(shardPaths) > 0 {
			return mergeShards(ctx, shardPaths, entryPath, outputPath, shardOutputPath, extensionURI)
		}

		if entryPath == "" {
			return fmt.Errorf("missing --%s", flagEntry)
		}

		if shardOutputPath != "" && extensionURI == "" {
			return fmt.Errorf(
				"--%s requires --%s so the shard identifies the stats extension version",
				flagShardOutput, flagStatsExtension,
			)
		}

		rewriteEntry := outputPath != ""
		if rewriteEntry && extensionURI == "" {
			// online path: determine the latest released stats extension
			uri, err := latestStatsExtensionURI(ctx.Context)
			if err != nil {
				return err
			}
			extensionURI = uri
		}

		collector, err := stats.NewCollector(extensionURI)
		if err != nil {
			return err
		}

		var entryResource crawler.Resource
		skipEntry := rewriteEntry

		bar := progressbar.NewOptions64(
			-1,
			progressbar.OptionSetDescription("catalogs: 0; collections: 0; items: 0"),
			progressbar.OptionSetItsString("r"),
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionSetWidth(10),
			progressbar.OptionThrottle(65*time.Millisecond),
			progressbar.OptionShowIts(),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionFullWidth(),
			progressbar.OptionClearOnFinish(),
		)

		visitor := func(resource crawler.Resource, info *crawler.ResourceInfo) error {
			if skipEntry {
				// statistics describe the resources below the entry point
				entryResource = resource
				skipEntry = false
				return nil
			}

			if err := collector.Visit(resource, info); err != nil {
				return err
			}

			snapshot := collector.Snapshot()
			_ = bar.Add(1)
			bar.Describe(fmt.Sprintf(
				"catalogs: %d; collections: %d; items: %d",
				snapshot.Count(stats.Catalog),
				snapshot.Count(stats.Collection),
				snapshot.Count(stats.Item),
			))

			return nil
		}

		if err := crawler.Crawl(entryPath, visitor); err != nil {
			return err
		}

		_ = bar.Finish()

		if shardOutputPath != "" {
			shard, err := collector.Shard()
			if err != nil {
				return err
			}
			if err := stats.WriteShard(shardOutputPath, shard); err != nil {
				return err
			}
		}

		if rewriteEntry {
			return writeEntryWithStats(outputPath, entryResource, collector.Snapshot(), extensionURI)
		}

		if shardOutputPath != "" {
			return nil
		}

		return json.NewEncoder(ctx.App.Writer).Encode(collector.Snapshot())
	},
}

func latestStatsExtensionURI(ctx context.Context) (string, error) {
	client := github.NewClient(nil)
	release, _, releaseErr := client.Repositories.GetLatestRelease(ctx, statsRepoOwner, statsRepoName)
	if releaseErr != nil {
		return "", fmt.Errorf(
			"failed to get latest release information for https://github.com/%s/%s: %w",
			statsRepoOwner, statsRepoName, releaseErr,
		)
	}
	tag := release.GetTagName()
	if tag == "" {
		return "", fmt.Errorf(
			"latest release for https://github.com/%s/%s has no version identifier",
			statsRepoOwner, statsRepoName,
		)
	}
	return fmt.Sprintf("https://%s.github.io/%s/%s/schema.json", statsRepoOwner, statsRepoName, tag), nil
}

func mergeShards(
	ctx *cli.Context,
	shardPaths []string,
	entryPath string,
	outputPath string,
	shardOutputPath string,
	extensionURI string,
) error {
	if entryPath != "" && outputPath == "" {
		return fmt.Errorf("--%s is only used together with --%s when merging shards", flagEntry, flagOutput)
	}
	if outputPath != "" && entryPath == "" {
		return fmt.Errorf("--%s requires --%s when merging shards", flagOutput, flagEntry)
	}

	shards := make([]*stats.Shard, 0, len(shardPaths))
	for _, path := range shardPaths {
		shard, err := stats.ReadShard(path)
		if err != nil {
			return err
		}
		shards = append(shards, shard)
	}

	merged, err := stats.Merge(shards...)
	if err != nil {
		return err
	}

	if extensionURI != "" && extensionURI != merged.Extension {
		return fmt.Errorf(
			"stats extension conflict: --%s is %q but the shards use %q",
			flagStatsExtension, extensionURI, merged.Extension,
		)
	}

	if shardOutputPath != "" {
		if err := stats.WriteShard(shardOutputPath, merged); err != nil {
			return err
		}
	}

	if outputPath != "" {
		entryResource, err := loadEntryResource(entryPath)
		if err != nil {
			return err
		}
		return writeEntryWithStats(outputPath, entryResource, merged.Stats, merged.Extension)
	}

	if shardOutputPath != "" {
		return nil
	}

	return json.NewEncoder(ctx.App.Writer).Encode(merged.Stats)
}

func loadEntryResource(entryPath string) (crawler.Resource, error) {
	var data []byte
	if strings.HasPrefix(entryPath, "http://") || strings.HasPrefix(entryPath, "https://") {
		response, err := http.Get(entryPath)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch entry %s: %w", entryPath, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected response for %s: %d", entryPath, response.StatusCode)
		}
		data, err = io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read entry %s: %w", entryPath, err)
		}
	} else {
		var err error
		data, err = os.ReadFile(entryPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read entry %s: %w", entryPath, err)
		}
	}

	resource := crawler.Resource{}
	if err := json.Unmarshal(data, &resource); err != nil {
		return nil, fmt.Errorf("failed to parse entry %s: %w", entryPath, err)
	}
	return resource, nil
}

func cloneValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		clone := make(map[string]interface{}, len(v))
		for key, item := range v {
			clone[key] = cloneValue(item)
		}
		return clone
	case []interface{}:
		clone := make([]interface{}, len(v))
		for i, item := range v {
			clone[i] = cloneValue(item)
		}
		return clone
	default:
		return value
	}
}

// writeEntryWithStats writes a copy of the entry resource augmented with the
// stats extension and statistics.  The original resource is not modified.
func writeEntryWithStats(outputPath string, original crawler.Resource, summary *stats.Stats, extensionURI string) error {
	tag, err := stats.ExtensionVersion(extensionURI)
	if err != nil {
		return err
	}
	extensionRoot := strings.TrimSuffix(extensionURI, tag+"/schema.json")

	resource := crawler.Resource(cloneValue(map[string]interface{}(original)).(map[string]interface{}))

	extensions := []string{extensionURI}
	for _, extension := range resource.Extensions() {
		if strings.HasPrefix(extension, extensionRoot) {
			continue
		}
		extensions = append(extensions, extension)
	}

	resource["stac_extensions"] = extensions
	if summary.Catalogs != nil {
		resource["stats:catalogs"] = summary.Catalogs
	}
	if summary.Collections != nil {
		resource["stats:collections"] = summary.Collections
	}
	if summary.Items != nil {
		resource["stats:items"] = summary.Items
	}

	data, err := json.MarshalIndent(orderedMap(resource), "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode resource as JSON: %w", err)
	}
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write output %s: %w", outputPath, err)
	}
	return nil
}
