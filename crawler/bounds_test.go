package crawler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/planetlabs/go-stac/crawler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphJSON renders a minimal STAC catalog document.
func graphJSON(id string, links [][2]string) []byte {
	type linkDoc struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	}
	type catalogDoc struct {
		StacVersion string    `json:"stac_version"`
		Type        string    `json:"type"`
		ID          string    `json:"id"`
		Links       []linkDoc `json:"links"`
	}
	doc := catalogDoc{StacVersion: "1.0.0", Type: "Catalog", ID: id}
	for _, link := range links {
		doc.Links = append(doc.Links, linkDoc{Rel: link[0], Href: link[1]})
	}
	data, _ := json.Marshal(doc)
	return data
}

// newGraphServer serves catalogs keyed by path.  Each graph value is the
// node id followed by its child links ([rel, href] pairs).
func newGraphServer(t *testing.T, graph map[string][][2]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links, ok := graph[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(graphJSON(strings.TrimPrefix(r.URL.Path, "/"), links))
	}))
}

// childLinks builds [rel, href] pairs for child links to the given names.
func childLinks(names ...string) [][2]string {
	links := [][2]string{}
	for _, name := range names {
		links = append(links, [2]string{"child", "/" + name})
	}
	return links
}

type crawlResult struct {
	visits  map[string]int
	summary crawler.Summary
}

func runCrawl(t *testing.T, entry string, queue crawler.Queue, options ...*crawler.Options) crawlResult {
	t.Helper()

	mu := &sync.Mutex{}
	visits := map[string]int{}
	visitor := func(resource crawler.Resource, info *crawler.ResourceInfo) error {
		mu.Lock()
		visits[normalizeLocation(t, info.Location)]++
		mu.Unlock()
		return nil
	}

	opts := append([]*crawler.Options{}, options...)
	if queue != nil {
		opts = append(opts, &crawler.Options{Queue: queue})
	}

	c, err := crawler.New(visitor, opts...)
	require.NoError(t, err)

	require.NoError(t, c.Add(entry))
	require.NoError(t, c.Wait())

	return crawlResult{visits: visits, summary: c.Summary()}
}

func normalizeLocation(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || u.Scheme == "" {
		return location
	}
	u.Fragment = ""
	u.RawQuery = u.Query().Encode()
	return u.String()
}

func resultKeys(result crawlResult) []string {
	keys := make([]string, 0, len(result.visits))
	for key := range result.visits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func intValue(v int) *int {
	return &v
}

func TestBoundsMaxDepth(t *testing.T) {
	graph := map[string][][2]string{
		"/root": childLinks("a", "b"),
		"/a":    childLinks("a1"),
		"/b":    childLinks("b1"),
		"/a1":   {},
		"/b1":   {},
	}
	server := newGraphServer(t, graph)
	defer server.Close()
	entry := server.URL + "/root"
	rootURL := normalizeLocation(t, entry)

	t.Run("root only", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(0), DeduplicateLocations: true},
		})
		assert.Equal(t, []string{rootURL}, resultKeys(result))
		assert.Equal(t, 1, result.summary.Visited)
		assert.Equal(t, 2, result.summary.Skipped)
		assert.Equal(t, 0, result.summary.DuplicateLocations)
		assert.Equal(t, 0, result.summary.MaxDepth)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})

	t.Run("root and children", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(1), DeduplicateLocations: true},
		})
		assert.Len(t, result.visits, 3)
		assert.Equal(t, 3, result.summary.Visited)
		assert.Equal(t, 2, result.summary.Skipped)
		assert.Equal(t, 1, result.summary.MaxDepth)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})

	t.Run("full graph", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(2), DeduplicateLocations: true},
		})
		assert.Len(t, result.visits, 5)
		assert.Equal(t, 5, result.summary.Visited)
		assert.Equal(t, 0, result.summary.Skipped)
		assert.Equal(t, 2, result.summary.MaxDepth)
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})

	t.Run("custom root depth", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{RootDepth: 1, MaxDepth: intValue(1)},
		})
		assert.Equal(t, 1, result.summary.Visited)
		assert.Equal(t, 2, result.summary.Skipped)
		assert.Equal(t, 1, result.summary.MaxDepth)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})
}

func TestBoundsMaxDepthAPI(t *testing.T) {
	entry := "testdata/v1.0.0/api-catalog.json"

	t.Run("root only", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(0)},
		})
		assert.Equal(t, 1, result.summary.Visited)
		assert.Equal(t, 0, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})

	t.Run("root and collection", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(1)},
		})
		assert.Equal(t, 2, result.summary.Visited)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})

	t.Run("through items", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(2)},
		})
		assert.Equal(t, 3, result.summary.Visited)
		assert.Equal(t, 0, result.summary.Skipped)
		assert.Equal(t, 2, result.summary.MaxDepth)
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})
}

func TestBoundsMaxResources(t *testing.T) {
	graph := map[string][][2]string{
		"/root": childLinks("a", "b", "c", "d"),
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		graph["/"+name] = [][2]string{}
	}
	server := newGraphServer(t, graph)
	defer server.Close()
	entry := server.URL + "/root"
	rootURL := normalizeLocation(t, entry)

	t.Run("budget smaller than frontier", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxResources: intValue(3)},
		})
		assert.Equal(t, []string{
			server.URL + "/a",
			server.URL + "/b",
			rootURL,
		}, resultKeys(result))
		assert.Equal(t, 3, result.summary.Visited)
		assert.Equal(t, 2, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonMaxResources, result.summary.StopReason)
	})

	t.Run("budget of one", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxResources: intValue(1)},
		})
		assert.Equal(t, []string{rootURL}, resultKeys(result))
		assert.Equal(t, 1, result.summary.Visited)
		assert.Equal(t, 4, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonMaxResources, result.summary.StopReason)
	})

	t.Run("budget matches graph size", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxResources: intValue(5)},
		})
		assert.Equal(t, 5, result.summary.Visited)
		assert.Equal(t, 0, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})
}

func TestBoundsCycleDedup(t *testing.T) {
	t.Run("two node cycle", func(t *testing.T) {
		graph := map[string][][2]string{
			"/a": childLinks("b"),
			"/b": childLinks("a"),
		}
		server := newGraphServer(t, graph)
		defer server.Close()

		result := runCrawl(t, server.URL+"/a", nil, &crawler.Options{
			Bounds: &crawler.Bounds{DeduplicateLocations: true},
		})
		assert.Len(t, result.visits, 2)
		assert.Equal(t, 2, result.summary.Visited)
		assert.Equal(t, 1, result.summary.DuplicateLocations)
		assert.Equal(t, 0, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})

	t.Run("self loop", func(t *testing.T) {
		graph := map[string][][2]string{
			"/a": childLinks("a"),
		}
		server := newGraphServer(t, graph)
		defer server.Close()

		result := runCrawl(t, server.URL+"/a", nil, &crawler.Options{
			Bounds: &crawler.Bounds{DeduplicateLocations: true},
		})
		assert.Len(t, result.visits, 1)
		assert.Equal(t, 1, result.summary.DuplicateLocations)
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})

	t.Run("cycle stopped by depth bound without dedup", func(t *testing.T) {
		graph := map[string][][2]string{
			"/a": childLinks("b"),
			"/b": childLinks("a"),
		}
		server := newGraphServer(t, graph)
		defer server.Close()

		result := runCrawl(t, server.URL+"/a", nil, &crawler.Options{
			Bounds: &crawler.Bounds{MaxDepth: intValue(2)},
		})
		assert.Equal(t, 3, result.summary.Visited)
		assert.Equal(t, 1, result.summary.Skipped)
		assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
	})
}

func TestBoundsDuplicateLinks(t *testing.T) {
	graph := map[string][][2]string{
		"/root": {
			{"child", "/a"},
			{"child", "./a"},
			{"child", "/a?x=1&y=2"},
			{"child", "/a?y=2&x=1#section"},
		},
		"/a": {},
	}
	server := newGraphServer(t, graph)
	defer server.Close()
	entry := server.URL + "/root"
	aPlain := normalizeLocation(t, server.URL+"/a")
	aQuery := normalizeLocation(t, server.URL+"/a?x=1&y=2")

	t.Run("deduplicated", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{
			Bounds: &crawler.Bounds{DeduplicateLocations: true},
		})
		assert.Len(t, result.visits, 3)
		assert.Equal(t, 3, result.summary.Visited)
		assert.Equal(t, 2, result.summary.DuplicateLocations)
		assert.Equal(t, 1, result.visits[aPlain])
		assert.Equal(t, 1, result.visits[aQuery])
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})

	t.Run("legacy behavior when disabled", func(t *testing.T) {
		result := runCrawl(t, entry, nil, &crawler.Options{})
		assert.Equal(t, 5, result.summary.Visited)
		assert.Equal(t, 2, result.visits[aPlain])
		assert.Equal(t, 2, result.visits[aQuery])
		assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	})
}

func TestBoundsFilterInteraction(t *testing.T) {
	graph := map[string][][2]string{
		"/root":    childLinks("also", "keep", "skip-me"),
		"/also":    {},
		"/keep":    {},
		"/skip-me": {},
	}
	server := newGraphServer(t, graph)
	defer server.Close()
	entry := server.URL + "/root"

	result := runCrawl(t, entry, nil, &crawler.Options{
		Filter: func(location string) bool {
			return !strings.HasPrefix(location, server.URL+"/skip-me")
		},
		Bounds: &crawler.Bounds{MaxResources: intValue(3), DeduplicateLocations: true},
	})

	assert.Len(t, result.visits, 3)
	assert.Equal(t, 3, result.summary.Visited)
	assert.Equal(t, 1, result.summary.Skipped)
	assert.Equal(t, 0, result.summary.DuplicateLocations)
	assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
	_, skipped := result.visits[normalizeLocation(t, server.URL+"/skip-me")]
	assert.False(t, skipped)
}

func TestBoundsNaturalCompletion(t *testing.T) {
	result := runCrawl(t, "testdata/v1.0.0/catalog-with-collection-of-items.json", nil, &crawler.Options{
		Bounds: &crawler.Bounds{DeduplicateLocations: true},
	})
	assert.Equal(t, 3, result.summary.Visited)
	assert.Equal(t, 0, result.summary.Skipped)
	assert.Equal(t, 0, result.summary.DuplicateLocations)
	assert.Equal(t, 2, result.summary.MaxDepth)
	assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
}

func TestBoundsBudgetSkipsAreNotErrors(t *testing.T) {
	graph := map[string][][2]string{
		"/root": childLinks("a", "b", "c", "d"),
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		graph["/"+name] = [][2]string{}
	}
	server := newGraphServer(t, graph)
	defer server.Close()

	var mu sync.Mutex
	errors := []error{}
	c, err := crawler.New(
		func(resource crawler.Resource, info *crawler.ResourceInfo) error { return nil },
		&crawler.Options{
			Bounds: &crawler.Bounds{MaxResources: intValue(2)},
			ErrorHandler: func(err error) error {
				mu.Lock()
				errors = append(errors, err)
				mu.Unlock()
				return nil
			},
		},
	)
	require.NoError(t, err)
	require.NoError(t, c.Add(server.URL+"/root"))
	require.NoError(t, c.Wait())

	assert.Empty(t, errors)
	summary := c.Summary()
	assert.Equal(t, crawler.StopReasonMaxResources, summary.StopReason)
	assert.Equal(t, 2, summary.Visited)
	assert.Equal(t, 3, summary.Skipped)
}

func TestBoundsHandledErrorsAreNotSkips(t *testing.T) {
	var mu sync.Mutex
	errors := []error{}
	entry := "testdata/v1.0.0/catalog-with-one-invalid-collection.json"

	result := runCrawl(t, entry, nil, &crawler.Options{
		ErrorHandler: func(err error) error {
			mu.Lock()
			errors = append(errors, err)
			mu.Unlock()
			return nil
		},
		Bounds: &crawler.Bounds{DeduplicateLocations: true},
	})
	require.Len(t, errors, 1)
	assert.True(t, strings.HasPrefix(errors[0].Error(), "failed to parse"))
	assert.Equal(t, 3, result.summary.Visited)
	assert.Equal(t, 0, result.summary.Skipped)
	assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
}

func TestBoundsInvalidConfig(t *testing.T) {
	visitor := func(resource crawler.Resource, info *crawler.ResourceInfo) error { return nil }

	_, err := crawler.New(visitor, &crawler.Options{Bounds: &crawler.Bounds{MaxDepth: intValue(-1)}})
	require.Error(t, err)

	_, err = crawler.New(visitor, &crawler.Options{
		Bounds: &crawler.Bounds{RootDepth: 3, MaxDepth: intValue(2)},
	})
	require.Error(t, err)

	_, err = crawler.New(visitor, &crawler.Options{Bounds: &crawler.Bounds{MaxResources: intValue(-1)}})
	require.Error(t, err)
}

// diamondGraph has a shared grandchild (a1 linked from both a and b) and a
// duplicate link to c, giving 8 distinct resources and 2 duplicate
// encounters when deduplicating.
func diamondGraph() map[string][][2]string {
	return map[string][][2]string{
		"/root": childLinks("a", "b", "c", "c"),
		"/a":    childLinks("a1", "a2"),
		"/b":    childLinks("a1", "b1"),
		"/c":    childLinks("c1"),
		"/a1":   {},
		"/a2":   {},
		"/b1":   {},
		"/c1":   {},
	}
}

func TestBoundsConcurrencyDeterminism(t *testing.T) {
	server := newGraphServer(t, diamondGraph())
	defer server.Close()
	entry := server.URL + "/root"

	configs := []*crawler.Bounds{
		{MaxDepth: intValue(5), DeduplicateLocations: true},
		{MaxResources: intValue(5), DeduplicateLocations: true},
		{MaxResources: intValue(100), MaxDepth: intValue(2), DeduplicateLocations: true},
	}
	limits := []int{1, 2, 3, 8}

	for ci, bounds := range configs {
		var reference crawlResult
		for repeat := 0; repeat < 3; repeat++ {
			for _, limit := range limits {
				queue := crawler.NewMemoryQueue(context.Background(), limit)
				result := runCrawl(t, entry, queue, &crawler.Options{Bounds: bounds})
				if reference.visits == nil {
					reference = result
					continue
				}
				assert.Equal(t, resultKeys(reference), resultKeys(result),
					"config %d: visited set differs at concurrency %d", ci, limit)
				assert.Equal(t, reference.summary, result.summary,
					"config %d: summary differs at concurrency %d", ci, limit)
			}
		}
	}
}

func TestBoundsCustomQueue(t *testing.T) {
	server := newGraphServer(t, diamondGraph())
	defer server.Close()

	queue := newSerialQueue()
	result := runCrawl(t, server.URL+"/root", queue, &crawler.Options{
		Bounds: &crawler.Bounds{MaxDepth: intValue(1), DeduplicateLocations: true},
	})

	// Root plus a, b, c (the duplicate c link is deduplicated).
	assert.Equal(t, 4, result.summary.Visited)
	assert.Equal(t, 1, result.summary.DuplicateLocations)
	assert.Equal(t, 1, result.summary.MaxDepth)
	assert.Equal(t, crawler.StopReasonMaxDepth, result.summary.StopReason)
}

// serialQueue is a single-worker Queue implementation used to verify that
// bounds enforcement works with queue implementations other than the
// built-in memory queue.
type serialQueue struct {
	tasks    chan *crawler.Task
	handler  crawler.Handler
	wg       sync.WaitGroup
	start    sync.Once
	shutdown sync.Once
}

func newSerialQueue() *serialQueue {
	return &serialQueue{tasks: make(chan *crawler.Task, 10000)}
}

func (q *serialQueue) Add(tasks []*crawler.Task) error {
	q.wg.Add(len(tasks))
	for _, task := range tasks {
		q.tasks <- task
	}
	return nil
}

func (q *serialQueue) Handle(handler crawler.Handler) {
	q.handler = handler
	q.start.Do(func() {
		go func() {
			for task := range q.tasks {
				_ = q.handler(task)
				q.wg.Done()
			}
		}()
	})
}

func (q *serialQueue) Wait() error {
	q.wg.Wait()
	q.shutdown.Do(func() { close(q.tasks) })
	return nil
}

var _ crawler.Queue = (*serialQueue)(nil)

func TestBoundsSummaryIsEmptyBeforeCrawl(t *testing.T) {
	c, err := crawler.New(func(resource crawler.Resource, info *crawler.ResourceInfo) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, crawler.Summary{}, c.Summary())
}

func TestBoundsReadonlySummary(t *testing.T) {
	graph := map[string][][2]string{
		"/root": childLinks("a"),
		"/a":    {},
	}
	server := newGraphServer(t, graph)
	defer server.Close()

	result := runCrawl(t, server.URL+"/root", nil, &crawler.Options{
		Bounds: &crawler.Bounds{DeduplicateLocations: true},
	})

	// Mutating the returned summary must not affect the crawler.
	summary := result.summary
	summary.Visited = 999
	summary.StopReason = "bogus"
	assert.Equal(t, 2, result.summary.Visited)
	assert.Equal(t, crawler.StopReasonNatural, result.summary.StopReason)
}

func TestBoundsDepthAndBudget(t *testing.T) {
	server := newGraphServer(t, diamondGraph())
	defer server.Close()
	entry := server.URL + "/root"

	// Depth 1 admits root and three children; the budget allows only two.
	result := runCrawl(t, entry, nil, &crawler.Options{
		Bounds: &crawler.Bounds{MaxDepth: intValue(1), MaxResources: intValue(2), DeduplicateLocations: true},
	})
	assert.Equal(t, 2, result.summary.Visited)
	assert.Equal(t, crawler.StopReasonMaxResources, result.summary.StopReason)
	assert.Equal(t, []string{
		server.URL + "/a",
		normalizeLocation(t, entry),
	}, resultKeys(result))
}

func TestBoundsMultipleEntries(t *testing.T) {
	graph := map[string][][2]string{
		"/a":  childLinks("a1"),
		"/b":  childLinks("b1"),
		"/a1": {},
		"/b1": {},
	}
	server := newGraphServer(t, graph)
	defer server.Close()

	c, err := crawler.New(
		func(resource crawler.Resource, info *crawler.ResourceInfo) error { return nil },
		&crawler.Options{Bounds: &crawler.Bounds{DeduplicateLocations: true}},
	)
	require.NoError(t, err)
	require.NoError(t, c.Add(server.URL+"/a"))
	require.NoError(t, c.Add(server.URL+"/b"))
	require.NoError(t, c.Wait())

	summary := c.Summary()
	assert.Equal(t, 4, summary.Visited)
	assert.Equal(t, crawler.StopReasonNatural, summary.StopReason)
}
