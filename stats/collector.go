package stats

import (
	"fmt"
	"sync"

	"github.com/planetlabs/go-stac/crawler"
)

// Collector accumulates statistics from resources visited during a crawl.
//
// A Collector is safe for concurrent use and implements the crawler.Visitor
// function type, so it can be passed to crawler.Crawl:
//
//	collector, _ := stats.NewCollector(extensionURI)
//	err := crawler.Crawl(entry, collector.Visit)
//	shard, err := collector.Shard()
//
// Multiple collectors can be used to gather statistics for different
// partitions of a publication; the resulting shards can be merged with Merge.
type Collector struct {
	mu        sync.Mutex
	extension string
	stats     *Stats
}

// NewCollector creates a Collector.
//
// The extensionURI identifies the STAC Stats extension release that
// statistics target and is required to produce a shard (see Shard).  It may be
// empty when only a Stats summary is needed (see Snapshot).
func NewCollector(extensionURI string) (*Collector, error) {
	if extensionURI != "" {
		if _, err := ExtensionVersion(extensionURI); err != nil {
			return nil, err
		}
	}
	return &Collector{
		extension: extensionURI,
		stats:     &Stats{},
	}, nil
}

func increment(counts map[string]uint64, key string) map[string]uint64 {
	if counts == nil {
		counts = map[string]uint64{}
	}
	counts[key] += 1
	return counts
}

// observe counts a single resource.  The caller must hold c.mu.
func (c *Collector) observe(resource crawler.Resource) {
	var resourceStats **ResourceStats

	switch resource.Type() {
	case crawler.Catalog:
		if c.stats.Catalogs == nil {
			c.stats.Catalogs = &ResourceStats{Versions: map[string]uint64{}}
		}
		resourceStats = &c.stats.Catalogs
		for _, conformance := range resource.ConformsTo() {
			(*resourceStats).Conformance = increment((*resourceStats).Conformance, conformance)
		}

	case crawler.Collection:
		if c.stats.Collections == nil {
			c.stats.Collections = &ResourceStats{Versions: map[string]uint64{}}
		}
		resourceStats = &c.stats.Collections

	case crawler.Item:
		if c.stats.Items == nil {
			c.stats.Items = &ResourceStats{Versions: map[string]uint64{}}
		}
		resourceStats = &c.stats.Items
		for _, asset := range resource.Assets() {
			(*resourceStats).Assets = increment((*resourceStats).Assets, asset.Type())
		}

	default:
		return
	}

	for _, extension := range resource.Extensions() {
		(*resourceStats).Extensions = increment((*resourceStats).Extensions, extension)
	}
	(*resourceStats).Versions = increment((*resourceStats).Versions, resource.Version())
	(*resourceStats).Count += 1
}

// Visit is a crawler.Visitor that records statistics for each visited
// resource.  Resources whose type cannot be determined are ignored.
func (c *Collector) Visit(resource crawler.Resource, _ *crawler.ResourceInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(resource)
	return nil
}

// Count records a single resource.  It has the same effect as Visit but can
// be used without a crawler.ResourceInfo.
func (c *Collector) Count(resource crawler.Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(resource)
}

// Snapshot returns an independent copy of the statistics collected so far.
// Resource kinds that have not been observed are omitted.
func (c *Collector) Snapshot() *Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats.clone()
}

// Shard returns an independent, validated copy of the collected statistics as
// a versioned shard.  The collector must have been created with a STAC Stats
// extension URI.
func (c *Collector) Shard() (*Shard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.extension == "" {
		return nil, fmt.Errorf("cannot create a shard without a stats extension URI")
	}
	shard := &Shard{
		Version:   ShardVersion,
		Extension: c.extension,
		Stats:     c.stats.clone(),
	}
	if err := shard.Validate(); err != nil {
		return nil, err
	}
	return shard, nil
}
