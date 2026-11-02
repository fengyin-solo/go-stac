// Package stats provides collection and merging of STAC crawl statistics.
//
// Statistics can be collected independently for different partitions of a
// large STAC publication, serialized as versioned shards, and later merged
// into a single result.  Merging is purely additive, so the merged result
// does not depend on the order in which shards are provided.
package stats

// ResourceKind identifies a kind of STAC resource.
type ResourceKind string

const (
	// Catalog is the catalog resource kind.
	Catalog ResourceKind = "catalog"
	// Collection is the collection resource kind.
	Collection ResourceKind = "collection"
	// Item is the item resource kind.
	Item ResourceKind = "item"
)

// ResourceStats summarizes a set of resources of a single kind.
//
// The JSON structure matches the resource stats objects of the STAC Stats
// extension (https://github.com/stac-extensions/stats).
type ResourceStats struct {
	// Count is the number of observed resources.
	Count uint64 `json:"count"`

	// Versions counts resources by their stac_version value.
	Versions map[string]uint64 `json:"versions,omitempty"`

	// Extensions counts resources by the extension URIs they declare.
	Extensions map[string]uint64 `json:"extensions,omitempty"`

	// Conformance counts catalogs by the conformance classes they declare.
	Conformance map[string]uint64 `json:"conformance,omitempty"`

	// Assets counts the assets of items by their media type.
	Assets map[string]uint64 `json:"assets,omitempty"`
}

// Stats summarizes the catalogs, collections, and items observed during a
// crawl.  Resource kinds that were not observed are left nil so that zero
// counts are never reported as present.
type Stats struct {
	Catalogs    *ResourceStats `json:"stats:catalogs,omitempty"`
	Collections *ResourceStats `json:"stats:collections,omitempty"`
	Items       *ResourceStats `json:"stats:items,omitempty"`
}

func addCounts(dst, src map[string]uint64) map[string]uint64 {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]uint64, len(src))
	}
	for key, count := range src {
		dst[key] += count
	}
	return dst
}

func addResourceStats(dst **ResourceStats, src *ResourceStats) {
	if src == nil || src.Count == 0 {
		return
	}
	if *dst == nil {
		*dst = &ResourceStats{}
	}
	d := *dst
	d.Count += src.Count
	d.Versions = addCounts(d.Versions, src.Versions)
	d.Extensions = addCounts(d.Extensions, src.Extensions)
	d.Conformance = addCounts(d.Conformance, src.Conformance)
	d.Assets = addCounts(d.Assets, src.Assets)
}

// Add merges the counts of other into s.  Adding is commutative and
// associative: the resulting Stats is the same regardless of the order in
// which stats are added.  A nil other is ignored.
func (s *Stats) Add(other *Stats) {
	if other == nil {
		return
	}
	addResourceStats(&s.Catalogs, other.Catalogs)
	addResourceStats(&s.Collections, other.Collections)
	addResourceStats(&s.Items, other.Items)
}

func cloneCounts(counts map[string]uint64) map[string]uint64 {
	if counts == nil {
		return nil
	}
	clone := make(map[string]uint64, len(counts))
	for key, count := range counts {
		clone[key] = count
	}
	return clone
}

func (r *ResourceStats) clone() *ResourceStats {
	if r == nil {
		return nil
	}
	return &ResourceStats{
		Count:       r.Count,
		Versions:    cloneCounts(r.Versions),
		Extensions:  cloneCounts(r.Extensions),
		Conformance: cloneCounts(r.Conformance),
		Assets:      cloneCounts(r.Assets),
	}
}

func (s *Stats) clone() *Stats {
	if s == nil {
		return nil
	}
	return &Stats{
		Catalogs:    s.Catalogs.clone(),
		Collections: s.Collections.clone(),
		Items:       s.Items.clone(),
	}
}

// Count returns the number of observed resources of the given kind.  It
// returns 0 for resource kinds that were not observed.
func (s *Stats) Count(kind ResourceKind) uint64 {
	var rs *ResourceStats
	switch kind {
	case Catalog:
		rs = s.Catalogs
	case Collection:
		rs = s.Collections
	case Item:
		rs = s.Items
	}
	if rs == nil {
		return 0
	}
	return rs.Count
}
