package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
)

// ShardVersion is the version of the shard structure understood by this
// package.  Shards with any other version cannot be read or merged.
const ShardVersion uint64 = 1

// extensionURIPattern matches released STAC Stats extension schema URIs such
// as https://stac-extensions.github.io/stats/v0.2.0/schema.json.
var extensionURIPattern = regexp.MustCompile(
	`^https://stac-extensions\.github\.io/stats/(v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)/schema\.json$`,
)

// ExtensionRoot is the root URI shared by all STAC Stats extension releases.
const ExtensionRoot = "https://stac-extensions.github.io/stats/"

// ExtensionVersion returns the release tag (e.g. "v0.2.0") identified by a
// STAC Stats extension schema URI.  An error is returned for URIs that do not
// reference a released version of the STAC Stats extension.
func ExtensionVersion(uri string) (string, error) {
	match := extensionURIPattern.FindStringSubmatch(uri)
	if match == nil {
		return "", fmt.Errorf(
			"invalid stats extension URI %q: expected an https URI of the form %sv<version>/schema.json",
			uri, ExtensionRoot,
		)
	}
	return match[1], nil
}

// Shard is a versioned, verifiable unit of collected statistics.
//
// Shards are produced from a (possibly partial) crawl and can be merged with
// other shards.  The Version allows future changes to the structure to be
// detected, and the Extension identifies the STAC Stats extension release
// that the statistics target.
type Shard struct {
	// Version is the structure version.  It must equal ShardVersion.
	Version uint64 `json:"version"`

	// Extension is the URI of the STAC Stats extension schema that the shard
	// targets (e.g. https://stac-extensions.github.io/stats/v0.2.0/schema.json).
	Extension string `json:"extension"`

	// Stats holds the collected resource counts.
	Stats *Stats `json:"stats"`
}

// NewShard creates an empty shard with the given STAC Stats extension URI.
// The URI is validated before the shard is created.
func NewShard(extensionURI string) (*Shard, error) {
	if _, err := ExtensionVersion(extensionURI); err != nil {
		return nil, err
	}
	return &Shard{
		Version:   ShardVersion,
		Extension: extensionURI,
		Stats:     &Stats{},
	}, nil
}

func validateCounts(kind ResourceKind, field string, counts map[string]uint64, allowed bool) (uint64, error) {
	if len(counts) > 0 && !allowed {
		return 0, fmt.Errorf("field %q is not valid for %s stats", field, kind)
	}
	var sum uint64
	for key, count := range counts {
		if count == 0 {
			return 0, fmt.Errorf("%s stats have a zero %s count for %q; omit zero counts instead", kind, field, key)
		}
		sum += count
	}
	return sum, nil
}

// validateResourceStats checks the internal consistency of resource stats.
// Counts must be positive, version counts must add up to the resource count,
// per-resource counts must not exceed the resource count, and fields may only
// appear for the resource kinds that the STAC Stats extension defines them
// for (conformance for catalogs, assets for items).
func validateResourceStats(kind ResourceKind, rs *ResourceStats) error {
	if rs == nil {
		return nil
	}
	if rs.Count == 0 {
		return fmt.Errorf("%s stats are present with a zero count; omit %s stats instead of reporting a zero count", kind, kind)
	}

	versionSum, err := validateCounts(kind, "versions", rs.Versions, true)
	if err != nil {
		return err
	}
	if versionSum != rs.Count {
		return fmt.Errorf(
			"%s version counts (%d) do not add up to the resource count (%d)",
			kind, versionSum, rs.Count,
		)
	}

	extensionSum, err := validateCounts(kind, "extensions", rs.Extensions, true)
	if err != nil {
		return err
	}
	if extensionSum > rs.Count {
		return fmt.Errorf(
			"%s extension counts (%d) exceed the resource count (%d)",
			kind, extensionSum, rs.Count,
		)
	}

	conformanceAllowed := kind == Catalog
	conformanceSum, err := validateCounts(kind, "conformance", rs.Conformance, conformanceAllowed)
	if err != nil {
		return err
	}
	if conformanceSum > rs.Count {
		return fmt.Errorf(
			"%s conformance counts (%d) exceed the resource count (%d)",
			kind, conformanceSum, rs.Count,
		)
	}

	// assets are counted across all items, so their total is not bounded by
	// the number of items
	if _, err := validateCounts(kind, "assets", rs.Assets, kind == Item); err != nil {
		return err
	}

	return nil
}

// Validate checks that the shard has a supported version, a valid STAC Stats
// extension URI, and internally consistent statistics.
func (s *Shard) Validate() error {
	if s == nil {
		return fmt.Errorf("empty shard")
	}
	switch {
	case s.Version == 0:
		return fmt.Errorf("missing shard version: expected version %d", ShardVersion)
	case s.Version != ShardVersion:
		return fmt.Errorf("unsupported shard version %d: expected version %d", s.Version, ShardVersion)
	}
	if _, err := ExtensionVersion(s.Extension); err != nil {
		return err
	}
	if s.Stats == nil {
		return fmt.Errorf("shard is missing the stats object")
	}
	if err := validateResourceStats(Catalog, s.Stats.Catalogs); err != nil {
		return err
	}
	if err := validateResourceStats(Collection, s.Stats.Collections); err != nil {
		return err
	}
	if err := validateResourceStats(Item, s.Stats.Items); err != nil {
		return err
	}
	return nil
}

// Merge combines any number of validated shards into a single shard.
//
// All shards must use the supported structure version and the same STAC Stats
// extension URI, otherwise an error describing the conflict is returned.
// Counts are added together, so the merged result is identical regardless of
// the order in which shards are merged.
func Merge(shards ...*Shard) (*Shard, error) {
	if len(shards) == 0 {
		return nil, fmt.Errorf("no shards to merge")
	}

	merged := &Shard{
		Version:   ShardVersion,
		Extension: shards[0].Extension,
		Stats:     &Stats{},
	}

	for i, shard := range shards {
		if shard == nil {
			return nil, fmt.Errorf("invalid shard %d: shard is empty", i)
		}
		if err := shard.Validate(); err != nil {
			return nil, fmt.Errorf("invalid shard %d: %w", i, err)
		}
		if shard.Version != merged.Version {
			return nil, fmt.Errorf(
				"shard version conflict: shard 0 uses version %d, shard %d uses version %d",
				merged.Version, i, shard.Version,
			)
		}
		if i > 0 && shard.Extension != merged.Extension {
			return nil, fmt.Errorf(
				"stats extension conflict: shard 0 uses %q, shard %d uses %q",
				merged.Extension, i, shard.Extension,
			)
		}
		merged.Stats.Add(shard.Stats)
	}

	return merged, nil
}

// Marshal encodes the shard as indented JSON.  Map keys are sorted by the
// encoding/json package, making the output deterministic for equivalent
// statistics.
func (s *Shard) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s); err != nil {
		return nil, fmt.Errorf("failed to encode shard: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// LoadShard reads and validates a single shard.  Unknown JSON properties and
// unsupported structure versions are rejected.
func LoadShard(r io.Reader) (*Shard, error) {
	shard := &Shard{}
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(shard); err != nil {
		return nil, fmt.Errorf("failed to parse shard: %w", err)
	}
	if err := shard.Validate(); err != nil {
		return nil, err
	}
	return shard, nil
}

// ReadShard reads and validates a shard from a file path.
func ReadShard(path string) (*Shard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read shard %s: %w", path, err)
	}
	shard, err := LoadShard(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to load shard %s: %w", path, err)
	}
	return shard, nil
}

// WriteShard validates and writes a shard to a file path.
func WriteShard(path string, shard *Shard) error {
	if err := shard.Validate(); err != nil {
		return err
	}
	data, err := shard.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write shard %s: %w", path, err)
	}
	return nil
}
