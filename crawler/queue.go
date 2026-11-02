package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/planetlabs/go-stac/internal/normurl"
)

type taskType string

const (
	resourceTask    = taskType("resource")
	collectionsTask = taskType("collections")
	childrenTask    = taskType("children")
	featuresTask    = taskType("features")
	visitTask       = taskType("visit")
)

var validTaskTypes = map[taskType]bool{
	resourceTask:    true,
	collectionsTask: true,
	childrenTask:    true,
	featuresTask:    true,
	visitTask:       true,
}

type Task struct {
	entry    *normurl.Locator
	resource *normurl.Locator
	taskType taskType
	depth    int

	// data holds an already loaded resource for a visitTask.  When nil, the
	// resource is loaded from the task's resource location.  Data is not
	// relied upon for tasks serialized through an external queue.
	data Resource

	// strictItems marks a visitTask for a collection discovered in a
	// collections listing, which is expected to link to its items.
	strictItems bool
}

func (t *Task) Entry() string {
	if t.entry == nil {
		return ""
	}
	return t.entry.String()
}

func (t *Task) Resource() string {
	if t.resource == nil {
		return ""
	}
	return t.resource.String()
}

// Depth returns the traversal depth assigned to the task.
func (t *Task) Depth() int {
	return t.depth
}

// new creates a task for the given resource at the same depth.
func (t *Task) new(resource *normurl.Locator, taskType taskType) *Task {
	return &Task{
		entry:    t.entry,
		resource: resource,
		taskType: taskType,
		depth:    t.depth,
	}
}

// child creates a task for a linked resource at depth + 1.
func (t *Task) child(resource *normurl.Locator, taskType taskType) *Task {
	child := t.new(resource, taskType)
	child.depth = t.depth + 1
	return child
}

// visit creates a task that visits an already loaded resource at depth + 1.
func (t *Task) visit(resource *normurl.Locator, data Resource, strictItems bool) *Task {
	child := t.child(resource, visitTask)
	child.data = data
	child.strictItems = strictItems
	return child
}

type jsonTask struct {
	Entry       *normurl.Locator
	Resource    *normurl.Locator
	Type        string
	Depth       int
	Data        *Resource
	StrictItems bool
}

func (t *Task) UnmarshalJSON(data []byte) error {
	var jt jsonTask
	if err := json.Unmarshal(data, &jt); err != nil {
		return err
	}

	t.entry = jt.Entry
	if t.entry == nil {
		return fmt.Errorf("missing entry")
	}

	t.resource = jt.Resource
	if t.resource == nil {
		return fmt.Errorf("missing resource")
	}

	t.taskType = taskType(jt.Type)
	if !validTaskTypes[t.taskType] {
		return fmt.Errorf("invalid task type: %s", t.taskType)
	}

	t.depth = jt.Depth
	t.strictItems = jt.StrictItems
	if jt.Data != nil {
		t.data = *jt.Data
	}

	return nil
}

func (t *Task) MarshalJSON() ([]byte, error) {
	jt := jsonTask{
		Entry:       t.entry,
		Resource:    t.resource,
		Type:        string(t.taskType),
		Depth:       t.depth,
		StrictItems: t.strictItems,
	}
	if t.data != nil {
		data := t.data
		jt.Data = &data
	}
	return json.Marshal(jt)
}

type Handler func(task *Task) error

type Queue interface {
	Add(tasks []*Task) error
	Handle(handler Handler)
	Wait() error
}

// NewMemoryQueue is used if a custom queue is not provided for a crawl.
//
// The crawl will stop if the provided context is cancelled.  The limit is used
// to control the number of resources that will be visited concurrently.  A
// limit of zero or less means there is no concurrency limit.
func NewMemoryQueue(ctx context.Context, limit int) Queue {
	ctx, cancel := context.WithCancel(ctx)
	q := &memoryQueue{
		ctx:     ctx,
		cancel:  cancel,
		mutex:   &sync.Mutex{},
		buffer:  []*Task{},
		handler: nil,
	}
	if limit > 0 {
		q.slots = make(chan struct{}, limit)
	}
	return q
}

type memoryQueue struct {
	ctx     context.Context
	cancel  context.CancelFunc
	slots   chan struct{}
	wg      sync.WaitGroup
	errOnce sync.Once
	err     error
	mutex   *sync.Mutex
	buffer  []*Task
	handler Handler
}

func (q *memoryQueue) Add(tasks []*Task) error {
	q.mutex.Lock()
	q.buffer = append(q.buffer, tasks...)
	q.mutex.Unlock()
	q.process()
	return nil
}

func (q *memoryQueue) Handle(handler Handler) {
	q.mutex.Lock()
	q.handler = handler
	q.mutex.Unlock()
	q.process()
}

func (q *memoryQueue) Wait() error {
	q.wg.Wait()
	return q.err
}

// acquireSlot reserves a concurrency slot.  A nil channel means the
// concurrency limit is unlimited.  Callers must hold q.mutex.
func (q *memoryQueue) acquireSlot() bool {
	if q.slots == nil {
		return true
	}
	select {
	case q.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (q *memoryQueue) process() {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	if q.handler == nil || q.ctx.Err() != nil {
		return
	}

	launched := []*Task{}
	for len(q.buffer) > 0 {
		if !q.acquireSlot() {
			break
		}
		task := q.buffer[0]
		q.buffer = q.buffer[1:]
		launched = append(launched, task)
	}

	for _, task := range launched {
		task := task
		q.wg.Add(1)
		go func() {
			// Release the slot, then refill the queue, and only mark this
			// task done after replacement tasks have been added to the wait
			// group so that Wait cannot return while buffered tasks remain.
			defer func() {
				if q.slots != nil {
					<-q.slots
				}
				q.process()
				q.wg.Done()
			}()
			if err := q.handler(task); err != nil {
				q.errOnce.Do(func() {
					q.err = err
					q.cancel()
				})
			}
		}()
	}
}

var _ Queue = (*memoryQueue)(nil)
