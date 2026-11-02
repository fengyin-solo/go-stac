package crawler

import (
	"fmt"
	"sort"
	"sync"
)

// StopReason describes why a crawl stopped.
type StopReason string

const (
	// StopReasonNatural means every reachable resource was visited.
	StopReasonNatural StopReason = "natural"

	// StopReasonMaxDepth means traversal stopped because links beyond the
	// configured maximum depth were not followed.
	StopReasonMaxDepth StopReason = "max-depth"

	// StopReasonMaxResources means traversal stopped because the configured
	// maximum number of resources was reached.
	StopReasonMaxResources StopReason = "max-resources"
)

// Bounds configures optional bounded traversal.
//
// A nil *Bounds leaves the crawl unbounded, matching the default crawler
// behavior.
type Bounds struct {
	// RootDepth is the depth assigned to resources added with Crawler.Add.
	// Resources linked from a resource at depth d are visited at depth d+1.
	// Defaults to 0.
	RootDepth int

	// MaxDepth limits the greatest depth of a visited resource.  Resources
	// linked from a resource at MaxDepth are not visited.  When nil, depth is
	// not limited.
	MaxDepth *int

	// MaxResources limits the number of resources admitted for a visit.
	// When nil, the number of resources is not limited.
	MaxResources *int

	// DeduplicateLocations ensures each resource, identified by its
	// normalized location, is visited at most once.  Enabling this makes
	// traversal terminate safely in the presence of cycles and repeated
	// links.
	DeduplicateLocations bool
}

func (b *Bounds) validate() error {
	if b == nil {
		return nil
	}
	if b.MaxDepth != nil && *b.MaxDepth < 0 {
		return fmt.Errorf("Bounds.MaxDepth cannot be negative")
	}
	if b.MaxDepth != nil && *b.MaxDepth < b.RootDepth {
		return fmt.Errorf("Bounds.MaxDepth (%d) cannot be less than Bounds.RootDepth (%d)", *b.MaxDepth, b.RootDepth)
	}
	if b.MaxResources != nil && *b.MaxResources < 0 {
		return fmt.Errorf("Bounds.MaxResources cannot be negative")
	}
	return nil
}

// Summary is a read-only snapshot of crawl progress and outcome.
type Summary struct {
	// Visited is the number of resources for which the visitor was called.
	Visited int

	// Skipped is the number of known resources that were not visited because
	// they were filtered out or excluded by MaxDepth or MaxResources.
	Skipped int

	// DuplicateLocations is the number of tasks targeting a normalized
	// location that was already encountered.
	DuplicateLocations int

	// MaxDepth is the greatest depth of a visited resource.
	MaxDepth int

	// StopReason indicates why the crawl stopped.  It is the empty string
	// while a crawl is in progress.
	StopReason StopReason
}

// isResourceTask reports whether processing the task calls the visitor.
func isResourceTask(t *Task) bool {
	return t.taskType == resourceTask || t.taskType == visitTask
}

// scheduler mediates task admission so that depth limits, the resource
// budget, location deduplication, and filters are applied before a task is
// handed to the queue.
//
// Admission proceeds breadth first: tasks at depth d are admitted before any
// task at depth d+1, and tasks within a depth are admitted in a deterministic
// order (sorted by normalized location).  This makes the visited resource set
// and the summary independent of the queue's concurrency.
type scheduler struct {
	mu     sync.Mutex
	bounds *Bounds
	filter func(string) bool

	seen map[string]struct{}

	// waiting holds admitted candidates per depth, sorted by normalized
	// location.
	waiting map[int][]*Task

	// active is the number of running tasks per depth.
	active map[int]int

	// reserved is the number of resource-bearing tasks admitted against
	// MaxResources.
	reserved int

	visited       int
	skipped       int
	duplicates    int
	maxDepth      int
	depthLimited  bool
	budgetReached bool
	finalized     bool
	stopReason    StopReason

	enqueue func([]*Task) error
}

func newScheduler(bounds *Bounds, filter func(string) bool, enqueue func([]*Task) error) *scheduler {
	return &scheduler{
		bounds:  bounds,
		filter:  filter,
		seen:    map[string]struct{}{},
		waiting: map[int][]*Task{},
		active:  map[int]int{},
		enqueue: enqueue,
	}
}

// submit classifies new tasks (e.g., from Crawler.Add) and dispatches any
// that are admitted.
func (s *scheduler) submit(tasks []*Task) error {
	s.mu.Lock()
	for _, t := range tasks {
		s.classify(t)
	}
	admitted := s.dispatchLocked()
	s.mu.Unlock()

	if len(admitted) > 0 {
		return s.enqueue(admitted)
	}
	return nil
}

// complete records the end of a running task, classifies the tasks it
// produced, and dispatches any that are admitted.
func (s *scheduler) complete(task *Task, children []*Task) error {
	s.mu.Lock()
	s.active[task.depth]--
	if s.active[task.depth] == 0 {
		delete(s.active, task.depth)
	}
	for _, child := range children {
		s.classify(child)
	}
	admitted := s.dispatchLocked()
	s.mu.Unlock()

	if len(admitted) > 0 {
		return s.enqueue(admitted)
	}
	return nil
}

// fail records that a running task stopped the crawl with an error.
func (s *scheduler) fail(task *Task) {
	s.mu.Lock()
	s.active[task.depth]--
	if s.active[task.depth] == 0 {
		delete(s.active, task.depth)
	}
	s.mu.Unlock()
}

// noteVisited records a visitor call for a resource at the given depth.
func (s *scheduler) noteVisited(depth int) {
	s.mu.Lock()
	s.visited++
	if depth > s.maxDepth {
		s.maxDepth = depth
	}
	s.mu.Unlock()
}

func (s *scheduler) snapshot() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Summary{
		Visited:            s.visited,
		Skipped:            s.skipped,
		DuplicateLocations: s.duplicates,
		MaxDepth:           s.maxDepth,
		StopReason:         s.stopReason,
	}
}

// classify applies depth, deduplication, filter, and budget gates to a task.
// Callers must hold s.mu.
func (s *scheduler) classify(t *Task) {
	if s.budgetReached {
		if isResourceTask(t) {
			s.skipped++
		}
		return
	}

	if s.bounds != nil && s.bounds.MaxDepth != nil {
		limit := *s.bounds.MaxDepth
		if isResourceTask(t) {
			if t.depth > limit {
				s.skipped++
				s.depthLimited = true
				return
			}
		} else {
			// A listing task at depth d only yields resources at d+1.
			if t.depth >= limit {
				s.depthLimited = true
				return
			}
		}
	}

	if s.bounds != nil && s.bounds.DeduplicateLocations {
		key := t.resource.Normalized()
		if _, ok := s.seen[key]; ok {
			s.duplicates++
			return
		}
		s.seen[key] = struct{}{}
	}

	if s.filter != nil && !s.filter(t.resource.String()) {
		s.skipped++
		return
	}

	bucket := append(s.waiting[t.depth], t)
	sort.SliceStable(bucket, func(i, j int) bool {
		return bucket[i].resource.Normalized() < bucket[j].resource.Normalized()
	})
	s.waiting[t.depth] = bucket
}

// dispatchLocked admits waiting tasks in breadth-first, location-sorted
// order.  A deeper depth is only dispatched once every task at shallower
// depths has completed.  Callers must hold s.mu.
func (s *scheduler) dispatchLocked() []*Task {
	admitted := []*Task{}
	for {
		depth := s.shallowestWaitingLocked()
		if depth < 0 {
			break
		}
		if s.activeAboveLocked(depth) > 0 {
			break
		}

		bucket := s.waiting[depth]
		delete(s.waiting, depth)

		for i, t := range bucket {
			if isResourceTask(t) && s.bounds != nil && s.bounds.MaxResources != nil && s.reserved >= *s.bounds.MaxResources {
				// Budget exhausted: this task and everything still waiting
				// is excluded.
				for _, excluded := range bucket[i:] {
					if isResourceTask(excluded) {
						s.skipped++
					}
				}
				s.drainWaitingLocked()
				s.budgetReached = true
				s.finalizeLocked()
				return admitted
			}

			if isResourceTask(t) {
				s.reserved++
				if t.depth > s.maxDepth {
					s.maxDepth = t.depth
				}
			}
			s.active[depth]++
			admitted = append(admitted, t)
		}
	}
	s.finalizeLocked()
	return admitted
}

// drainWaitingLocked counts remaining resource tasks as skipped and drops
// remaining listing tasks.  Callers must hold s.mu.
func (s *scheduler) drainWaitingLocked() {
	for depth, bucket := range s.waiting {
		for _, t := range bucket {
			if isResourceTask(t) {
				s.skipped++
			}
		}
		delete(s.waiting, depth)
	}
}

// finalizeLocked sets the stop reason once no tasks remain.  Callers must
// hold s.mu.
func (s *scheduler) finalizeLocked() {
	if s.finalized || len(s.waiting) > 0 || len(s.active) > 0 {
		return
	}
	s.finalized = true
	switch {
	case s.budgetReached:
		s.stopReason = StopReasonMaxResources
	case s.depthLimited:
		s.stopReason = StopReasonMaxDepth
	default:
		s.stopReason = StopReasonNatural
	}
}

// shallowestWaitingLocked returns the smallest depth with waiting tasks, or
// -1 when none are waiting.  Callers must hold s.mu.
func (s *scheduler) shallowestWaitingLocked() int {
	shallowest := 0
	found := false
	for depth := range s.waiting {
		if len(s.waiting[depth]) == 0 {
			continue
		}
		if !found || depth < shallowest {
			shallowest = depth
			found = true
		}
	}
	if !found {
		return -1
	}
	return shallowest
}

// activeAboveLocked reports the number of active tasks at depths shallower
// than the given depth.  Callers must hold s.mu.
func (s *scheduler) activeAboveLocked(depth int) int {
	count := 0
	for d, n := range s.active {
		if d < depth {
			count += n
		}
	}
	return count
}
