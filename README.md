# STAC Utilities

Utilities for working with Spatio-Temporal Asset Catalog ([STAC](https://stacspec.org/)) resources.

[![Go Reference](https://pkg.go.dev/badge/github.com/planetlabs/go-stac.svg)](https://pkg.go.dev/github.com/planetlabs/go-stac)
![Tests](https://github.com/planetlabs/go-stac/actions/workflows/test.yml/badge.svg)

The `stac` command line utility can be used to crawl and validate STAC metadata.  In addition, the [`github.com/planetlabs/go-stac`](https://pkg.go.dev/github.com/planetlabs/go-stac) module can be used in Go projects.

## Command Line Interface

The `stac` program can be installed by downloading one of the archives from [the latest release](https://github.com/planetlabs/go-stac/releases).

Extract the archive and place the `stac` executable somewhere on your path.  See a list of available commands by running `stac` in your terminal.

Mac users can install the `stac` program with [`brew`](https://brew.sh/):

    brew install planetlabs/tap/go-stac

### CLI Usage

Run `stac help` to see a full list of commands and their arguments.  The primary `stac` commands are documented below.

#### stac validate

The `stac validate` command crawls STAC resources and validates them against the appropriate schema.

Example use:

    stac validate --entry path/to/catalog.json

The `--entry` can be a file path or URL pointing to a catalog, collection, or item.  By default, all catalogs, collections, and items linked from the entry point will be validated.  Use the `--no-recursion` option to validate a single resource without crawling to linked resources.  See `stac validate --help` for a full list of supported options.

#### stac stats

The `stac stats` command crawls STAC resources and prints out counts of resource type, versions, extensions, asset types, and conformance classes (for API endpoints).

Example use:

    stac stats --entry path/to/catalog.json

The `--entry` can be a file path or URL pointing to a catalog, collection, or item.  The stats output is a JSON object with top-level properties for catalog, collection, and item stats.

The structure of the output conforms with the schema of the [STAC Stats extension](https://github.com/stac-extensions/stats), so the results can be added to a STAC entrypoint to provide stats on child catalogs, collections, and items.  The `stac stats` command can write out a copy of the provided entrypoint with statistics added.

To write out a version of a catalog or collection that includes metadata for the STAC Stats extension, run the following:

    stac stats --entry path/to/catalog.json --output path/to/catalog-with-stats.json

By default, this checks GitHub for the latest release of the STAC Stats extension.  To work offline (or to make a release reproducible), provide the extension schema URI explicitly with `--stats-extension`.  No network access is needed, and the entry resource is not modified:

    stac stats \
      --entry path/to/catalog.json \
      --output path/to/catalog-with-stats.json \
      --stats-extension https://stac-extensions.github.io/stats/v0.2.0/schema.json

For large publications that are crawled in partitions, each partition can be written as a versioned, self-describing stats shard:

    stac stats \
      --entry path/to/partition/catalog.json \
      --stats-extension https://stac-extensions.github.io/stats/v0.2.0/schema.json \
      --shard-output path/to/shard.json

The shards can then be merged.  Provide `--shard` once per shard (the order does not matter, the result is always the same) and optionally add the merged statistics to an entry resource.  Merging shards never accesses the network:

    stac stats \
      --shard path/to/shard-1.json \
      --shard path/to/shard-2.json \
      --entry path/to/catalog.json \
      --output path/to/catalog-with-stats.json

Shards are validated before they are merged.  Shards with an unknown structure version, an invalid stats extension URI, or conflicting stats extension versions produce an error, and zero counts are never reported as present.

The same functionality is available to Go programs through the [`github.com/planetlabs/go-stac/stats`](https://pkg.go.dev/github.com/planetlabs/go-stac/stats) package, which provides a `Collector` for gathering statistics during a crawl and shard loading/merging helpers.

## Library Use

Install the module into your project.
```
go get github.com/planetlabs/go-stac
```

See the [reference documentation](https://pkg.go.dev/github.com/planetlabs/go-stac) for example usage in a Golang project.

## Development

See the [development doc](./development.md) for information on developing and releasing the `go-stac` library.
