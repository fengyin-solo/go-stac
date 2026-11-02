// Package crawler implements a STAC resource crawler.
package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/planetlabs/go-stac/internal/normurl"
	"github.com/tschaub/retry"
)

var httpClient = retryablehttp.NewClient()

func init() {
	httpClient.Logger = nil
}

// ErrStopRecursion is returned by the visitor when it wants to stop recursing.
var ErrStopRecursion = errors.New("stop recursion")

func load(entry *normurl.Locator, loc *normurl.Locator, value interface{}) error {
	if loc.IsFilepath() {
		if !entry.IsFilepath() {
			return fmt.Errorf("cannot crawl file %s in non-file mode", loc)
		}
		return loadFile(loc, value)
	}

	if entry.IsFilepath() {
		return fmt.Errorf("cannot crawl URL %s in file mode", loc)
	}
	return loadUrl(loc, value)
}

func loadFile(loc *normurl.Locator, value any) error {
	data, readErr := os.ReadFile(loc.String())
	if readErr != nil {
		return fmt.Errorf("failed to read file %s: %w", loc, readErr)
	}

	jsonErr := json.Unmarshal(data, value)
	if jsonErr != nil {
		return fmt.Errorf("failed to parse %s: %w", loc, jsonErr)
	}
	return nil
}

func loadUrl(loc *normurl.Locator, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*30)
	defer cancel()

	retries := 5

	return retry.Limit(ctx, retries, func(ctx context.Context, attempt int) error {
		err := tryLoadUrl(loc, value)
		if err == nil {
			return nil
		}

		// these come when parsing the response body
		if !errors.Is(err, syscall.ECONNRESET) {
			return retry.Stop(err)
		}

		jitter := time.Duration(rand.Float64()) * time.Second
		time.Sleep(time.Second*time.Duration(math.Pow(2, float64(attempt))) + jitter)
		return err
	})
}

func tryLoadUrl(loc *normurl.Locator, value any) error {
	resp, err := httpClient.Get(loc.String())
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected response for %s: %d", loc, resp.StatusCode)
	}

	jsonErr := json.NewDecoder(resp.Body).Decode(value)
	if jsonErr != nil {
		return fmt.Errorf("failed to parse %s: %w", loc, jsonErr)
	}
	return nil
}

// ResourceInfo includes information about how the resource was accessed.
type ResourceInfo struct {
	// Location is the URL or file path of the resource.
	Location string

	// Entry is the URL or file path of the initial resource that was crawled and pointed to this resource.
	Entry string
}

// Visitor is called for each resource during crawling.
//
// Any returned error will stop crawling and be returned by Crawl.
type Visitor func(Resource, *ResourceInfo) error

// ErrorHandler is called with any errors during a crawl.  If the function
// returns nil, the crawl will continue.  If the function returns an error,
// the crawl will stop.
type ErrorHandler func(error) error

func wrapErrorHandler(handler ErrorHandler) ErrorHandler {
	return func(err error) error {
		if err == nil {
			return nil
		}
		return handler(err)
	}
}

// Crawler crawls STAC resources.
type Crawler struct {
	visitor      Visitor
	queue        Queue
	errorHandler ErrorHandler
	scheduler    *scheduler
	rootDepth    int
}

// Options for creating a crawler.
type Options struct {
	// Optional function to limit which resources to crawl.  If provided, the function
	// will be called with the URL or absolute path to a resource before it is crawled.
	// If the function returns false, the resource will not be read and the visitor will
	// not be called.
	Filter func(string) bool

	// Optional function to handle any errors during the crawl.  By default, any error
	// will stop the crawl.  To continue crawling on error, provide a function that
	// returns nil.  The special ErrStopRecursion will stop the crawler from recursing deeper
	// but will not stop the crawl altogether.
	ErrorHandler ErrorHandler

	// Optional queue to use for crawling tasks.  If not provided, an in-memory queue
	// will be used.  When running a crawl across multiple processes, it can be useful
	// to provide a queue that is shared across processes.
	Queue Queue

	// Optional bounds for the traversal.  Use this to limit the traversal depth,
	// the number of visited resources, and to deduplicate normalized locations.
	// When nil, the traversal is unbounded.
	Bounds *Bounds
}

func applyOptions(options []*Options) *Options {
	o := &Options{}
	for _, option := range options {
		if option.Queue != nil {
			o.Queue = option.Queue
		}
		if option.Filter != nil {
			o.Filter = option.Filter
		}
		if option.ErrorHandler != nil {
			o.ErrorHandler = option.ErrorHandler
		}
		if option.Bounds != nil {
			o.Bounds = option.Bounds
		}
	}
	return o
}

// DefaultOptions used when creating a new crawler.
var DefaultOptions = &Options{
	ErrorHandler: func(err error) error { return err },
}

// New creates a crawler with the provided options (or DefaultOptions
// if none are provided).
//
// The visitor will be called for each resource added and for every additional
// resource linked from the initial entry.
func New(visitor Visitor, options ...*Options) (*Crawler, error) {
	opt := applyOptions(append([]*Options{DefaultOptions}, options...))

	if err := opt.Bounds.validate(); err != nil {
		return nil, err
	}

	queue := opt.Queue
	if queue == nil {
		queue = NewMemoryQueue(context.Background(), runtime.GOMAXPROCS(0))
	}

	c := &Crawler{
		visitor:      visitor,
		queue:        queue,
		errorHandler: wrapErrorHandler(opt.ErrorHandler),
	}
	rootDepth := 0
	if opt.Bounds != nil {
		rootDepth = opt.Bounds.RootDepth
	}
	c.scheduler = newScheduler(opt.Bounds, opt.Filter, func(tasks []*Task) error {
		return c.queue.Add(tasks)
	})
	c.rootDepth = rootDepth
	queue.Handle(c.handle)

	return c, nil
}

// Add a new resource entry to crawl.
//
// The resource can be a file path or a URL.
func (c *Crawler) Add(resource string) error {
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		return fmt.Errorf("failed to get working directory: %w", wdErr)
	}

	base, baseErr := normurl.New(fmt.Sprintf("%s%c", wd, os.PathSeparator))
	if baseErr != nil {
		return fmt.Errorf("failed to parse working directory: %w", baseErr)
	}

	loc, locErr := base.Resolve(resource)
	if locErr != nil {
		return locErr
	}

	return c.scheduler.submit([]*Task{{
		entry:    loc,
		resource: loc,
		taskType: resourceTask,
		depth:    c.rootDepth,
	}})
}

// Wait for a crawl to finish.
func (c *Crawler) Wait() error {
	return c.queue.Wait()
}

// Summary returns a read-only snapshot of crawl statistics.  The stop reason
// is finalized after Wait returns.
func (c *Crawler) Summary() Summary {
	return c.scheduler.snapshot()
}

// Crawl calls the visitor for each resolved resource.
//
// The resource can be a file path or a URL.  Any error returned by visitor
// will stop crawling and be returned by this function.  Context cancellation
// will also stop crawling and the context error will be returned.
//
// This is a shorthand for calling New, Add, and Wait when you only need to crawl
// a single entry.
func Crawl(resource string, visitor Visitor, options ...*Options) error {
	c, err := New(visitor, options...)
	if err != nil {
		return err
	}

	addErr := c.Add(resource)
	if addErr != nil {
		return addErr
	}

	return c.Wait()
}

// handle is the queue handler.  Tasks only reach this handler after passing
// the scheduler's depth, deduplication, filter, and resource budget gates.
func (c *Crawler) handle(t *Task) error {
	tasks, err := c.process(t)
	if err != nil {
		c.scheduler.fail(t)
		return err
	}
	return c.scheduler.complete(t, tasks)
}

func (c *Crawler) process(t *Task) ([]*Task, error) {
	switch t.taskType {
	case resourceTask:
		return c.crawlResource(t)
	case visitTask:
		return c.crawlVisit(t)
	case collectionsTask:
		return c.crawlCollections(t)
	case childrenTask:
		return c.crawlChildren(t)
	case featuresTask:
		return c.crawlFeatures(t)
	default:
		return nil, fmt.Errorf("unknown task type: %s", t.taskType)
	}
}

// callVisitor records the visit in the crawl summary and invokes the visitor.
func (c *Crawler) callVisitor(task *Task, resource Resource) error {
	c.scheduler.noteVisited(task.depth)
	info := &ResourceInfo{Entry: task.entry.String(), Location: task.resource.String()}
	return c.errorHandler(c.visitor(resource, info))
}

// itemsTask resolves the "items" link of a collection and returns the
// features task that lists its items.  The returned task may be nil when the
// collection has no items link.
func (c *Crawler) itemsTask(task *Task, resource Resource, strict bool) (*Task, error) {
	links := resource.Links()
	itemsLink := links.Rel("items", LinkTypeGeoJSON, LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
	if itemsLink == nil {
		if strict {
			return nil, c.errorHandler(fmt.Errorf("missing items link for collection in %s", task.resource.String()))
		}
		return nil, nil
	}
	linkLoc, err := task.resource.Resolve(itemsLink["href"])
	if err != nil {
		return nil, c.errorHandler(err)
	}
	linkLoc.SetQueryParam("limit", "250")
	return task.new(linkLoc, featuresTask), nil
}

func (c *Crawler) crawlResource(task *Task) ([]*Task, error) {
	resource := Resource{}
	loadErr := load(task.entry, task.resource, &resource)
	if loadErr != nil {
		return nil, c.errorHandler(loadErr)
	}

	if err := c.callVisitor(task, resource); err != nil {
		if errors.Is(err, ErrStopRecursion) {
			return nil, nil
		}
		return nil, err
	}

	links := resource.Links()
	// check if this looks like a STAC API root catalog that implements OGC API - Features
	if resource.Type() == Catalog && len(resource.ConformsTo()) > 1 {
		dataLink := links.Rel("data", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
		if dataLink != nil {
			linkLoc, err := task.resource.Resolve(dataLink["href"])
			if err != nil {
				return nil, c.errorHandler(err)
			}
			return []*Task{task.new(linkLoc, collectionsTask)}, nil
		}
	}

	// check if this looks like a STAC API root catalog that implements STAC API - Children
	if resource.Type() == Catalog && len(resource.ConformsTo()) > 1 {
		childrenLink := links.Rel("children", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
		if childrenLink != nil {
			linkLoc, err := task.resource.Resolve(childrenLink["href"])
			if err != nil {
				return nil, c.errorHandler(err)
			}
			return []*Task{task.new(linkLoc, childrenTask)}, nil
		}
	}

	if resource.Type() == Collection {
		// shortcut for "items" link
		itemsTask, itemsErr := c.itemsTask(task, resource, false)
		if itemsErr != nil {
			return nil, itemsErr
		}
		if itemsTask != nil {
			return []*Task{itemsTask}, nil
		}
	}

	tasks := []*Task{}
	for _, link := range links {
		rel := link["rel"]
		if rel == "item" || rel == "child" {
			if LinkTypeApplicationJSON(link) || LinkTypeAnyJSON(link) || LinkTypeNone(link) {
				linkLoc, err := task.resource.Resolve(link["href"])
				if err != nil {
					unhandledErr := c.errorHandler(err)
					if unhandledErr != nil {
						return nil, unhandledErr
					}
					continue
				}
				tasks = append(tasks, task.child(linkLoc, resourceTask))
			}
		}
	}

	return tasks, nil
}

// crawlVisit visits a resource that was embedded in a listing response.  The
// embedded resource is carried on the task; when it is missing (for example
// after a task was serialized through an external queue), the resource is
// loaded from its location.
func (c *Crawler) crawlVisit(task *Task) ([]*Task, error) {
	resource := task.data
	if resource == nil {
		resource = Resource{}
		if loadErr := load(task.entry, task.resource, &resource); loadErr != nil {
			return nil, c.errorHandler(loadErr)
		}
	}

	if err := c.callVisitor(task, resource); err != nil {
		if errors.Is(err, ErrStopRecursion) {
			return nil, nil
		}
		return nil, err
	}

	if resource.Type() == Collection {
		itemsTask, itemsErr := c.itemsTask(task, resource, task.strictItems)
		if itemsErr != nil {
			return nil, itemsErr
		}
		if itemsTask != nil {
			return []*Task{itemsTask}, nil
		}
	}

	return nil, nil
}

func (c *Crawler) crawlCollections(task *Task) ([]*Task, error) {
	response := &featureCollectionsResponse{}
	loadErr := load(task.entry, task.resource, response)
	if loadErr != nil {
		return nil, c.errorHandler(loadErr)
	}

	tasks := []*Task{}
	for i, resource := range response.Collections {
		if resource.Type() != Collection {
			unhandledErr := c.errorHandler(fmt.Errorf("expected collection at index %d, got %s", i, resource.Type()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}
		links := resource.Links()

		selfLink := links.Rel("self", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
		if selfLink == nil {
			unhandledErr := c.errorHandler(fmt.Errorf("missing self link for collection %d in %s", i, task.resource.String()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}
		selfLinkLoc, selfLinkErr := task.resource.Resolve(selfLink["href"])
		if selfLinkErr != nil {
			unhandledErr := c.errorHandler(selfLinkErr)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		tasks = append(tasks, task.visit(selfLinkLoc, resource, true))
	}

	nextLink := response.Links.Rel("next", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
	if nextLink != nil {
		linkLoc, err := task.resource.Resolve(nextLink["href"])
		if err != nil {
			unhandledErr := c.errorHandler(err)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			return tasks, nil
		}
		tasks = append(tasks, task.new(linkLoc, collectionsTask))
	}

	return tasks, nil
}

func (c *Crawler) crawlChildren(task *Task) ([]*Task, error) {
	response := &childrenResponse{}
	loadErr := load(task.entry, task.resource, response)
	if loadErr != nil {
		return nil, c.errorHandler(loadErr)
	}

	tasks := []*Task{}
	for i, resource := range response.Children {
		if resource.Type() != Catalog && resource.Type() != Collection {
			unhandledErr := c.errorHandler(fmt.Errorf("expected catalog or collection at index %d, got %s", i, resource.Type()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}
		links := resource.Links()

		selfLink := links.Rel("self", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
		if selfLink == nil {
			unhandledErr := c.errorHandler(fmt.Errorf("missing self link for %s %d in %s", resource.Type(), i, task.resource.String()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		selfLinkLoc, selfLinkErr := task.resource.Resolve(selfLink["href"])
		if selfLinkErr != nil {
			unhandledErr := c.errorHandler(selfLinkErr)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		tasks = append(tasks, task.child(selfLinkLoc, resourceTask))
	}

	nextLink := response.Links.Rel("next", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
	if nextLink != nil {
		linkLoc, err := task.resource.Resolve(nextLink["href"])
		if err != nil {
			unhandledErr := c.errorHandler(err)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			return tasks, nil
		}
		tasks = append(tasks, task.new(linkLoc, childrenTask))
	}

	return tasks, nil
}

func (c *Crawler) crawlFeatures(task *Task) ([]*Task, error) {
	response := &featureCollectionResponse{}
	loadErr := load(task.entry, task.resource, response)
	if loadErr != nil {
		return nil, c.errorHandler(loadErr)
	}

	tasks := []*Task{}
	for i, resource := range response.Features {
		if resource.Type() != Item {
			unhandledErr := c.errorHandler(fmt.Errorf("expected item at index %d, got %s", i, resource.Type()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		links := resource.Links()
		selfLink := links.Rel("self", LinkTypeGeoJSON, LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
		if selfLink == nil {
			unhandledErr := c.errorHandler(fmt.Errorf("missing self link for item %d in %s", i, task.resource.String()))
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		selfLinkLoc, selfLinkErr := task.resource.Resolve(selfLink["href"])
		if selfLinkErr != nil {
			unhandledErr := c.errorHandler(selfLinkErr)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			continue
		}

		tasks = append(tasks, task.visit(selfLinkLoc, resource, false))
	}

	nextLink := response.Links.Rel("next", LinkTypeApplicationJSON, LinkTypeAnyJSON, LinkTypeNone)
	if nextLink != nil {
		linkLoc, err := task.resource.Resolve(nextLink["href"])
		if err != nil {
			unhandledErr := c.errorHandler(err)
			if unhandledErr != nil {
				return nil, unhandledErr
			}
			return tasks, nil
		}
		tasks = append(tasks, task.new(linkLoc, featuresTask))
	}

	return tasks, nil
}
