package validator

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/planetlabs/go-stac/crawler"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Phase describes the stage at which a validation problem was found.
type Phase string

const (
	// PhaseRead means the resource could not be read from the local
	// filesystem or fetched over the network.
	PhaseRead Phase = "read"

	// PhaseParse means the resource was read but its contents could not
	// be parsed as JSON.
	PhaseParse Phase = "parse"

	// PhaseSchema means the resource did not validate against the core
	// STAC JSON Schema for its type and version.
	PhaseSchema Phase = "schema"

	// PhaseExtension means the resource did not validate against a
	// STAC extension JSON Schema declared in its stac_extensions.
	PhaseExtension Phase = "extension"

	// PhaseError means an unexpected problem occurred (for example, a JSON
	// Schema could not be loaded or compiled).  Such problems are never
	// silently ignored.
	PhaseError Phase = "error"
)

// Issue describes a single validation problem found for a resource.
//
// The JSON field names are stable and are intended to be consumed by other
// tools.  Fields that do not apply to a problem are omitted.
type Issue struct {
	// Location is the URL or file path of the resource.
	Location string `json:"location"`

	// ID is the "id" member of the resource, when available.
	ID string `json:"id,omitempty"`

	// Type is the STAC resource type (item, catalog, or collection), when known.
	Type string `json:"type,omitempty"`

	// STACVersion is the "stac_version" member of the resource, when available.
	STACVersion string `json:"stac_version,omitempty"`

	// Phase is the stage at which the problem was found.
	Phase Phase `json:"phase"`

	// SchemaURL is the location of the JSON Schema that was being validated
	// against (a core STAC schema or an extension schema), when applicable.
	SchemaURL string `json:"schema_url,omitempty"`

	// Message describes the problem.
	Message string `json:"message"`

	// InstanceLocation is the JSON pointer to the value within the resource
	// that failed validation (e.g. "/properties/datetime").
	InstanceLocation string `json:"instance_location,omitempty"`

	// KeywordLocation is the JSON pointer to the validating keyword within
	// the schema (e.g. "/properties/datetime/type").
	KeywordLocation string `json:"keyword_location,omitempty"`

	// AbsoluteKeywordLocation is the absolute location of the validating
	// keyword, including the schema URL.
	AbsoluteKeywordLocation string `json:"absolute_keyword_location,omitempty"`
}

// Report is the result of a non-fail-fast validation run.
type Report struct {
	// Valid is the number of distinct resources that were successfully validated.
	Valid int `json:"valid"`

	// Invalid is the number of distinct resource locations with at least one
	// problem, including resources that could not be read or parsed.
	Invalid int `json:"invalid"`

	// Issues describes every problem found, sorted by location and schema path
	// so that repeated runs over the same resources produce stable output.
	Issues []*Issue `json:"issues"`
}

// resourceIssue is the internal result of checking a single resource.  The raw
// field preserves the legacy fail-fast error so that Validate keeps its
// original behavior.
type resourceIssue struct {
	phase     Phase
	message   string
	schemaURL string
	schemaErr *jsonschema.ValidationError
	raw       error
}

// ValidateReport validates every resource reachable from the entry without
// stopping at the first invalid resource.
//
// Resources that fail validation are counted as invalid and their links are
// not followed, but crawling continues with any other linked resources.
// Failures while reading or parsing a resource are reported under the "read"
// or "parse" phases and are never mistaken for schema failures.  Assets are
// not fetched.
//
// The returned error is reserved for problems that stop the crawl itself
// (such as a failure to enqueue the entry or context cancellation).
func (v *Validator) ValidateReport(ctx context.Context, entry string) (*Report, error) {
	collector := &reportCollector{
		visited:   map[string]struct{}{},
		invalid:   map[string]struct{}{},
		issueKeys: map[string]struct{}{},
		issues:    []*Issue{},
	}
	c, err := crawler.New(v.reportVisitor(collector), &crawler.Options{
		Queue:        crawler.NewMemoryQueue(ctx, v.concurrency),
		ErrorHandler: collector.handleError,
	})
	if err != nil {
		return nil, err
	}
	if err := c.Add(entry); err != nil {
		return nil, err
	}
	if err := c.Wait(); err != nil {
		return nil, err
	}
	return collector.report(), nil
}

func (v *Validator) reportVisitor(c *reportCollector) crawler.Visitor {
	return func(resource crawler.Resource, info *crawler.ResourceInfo) error {
		v.logger.Info("validating resource", "resource", info.Location)
		if !c.beginResource(info.Location) {
			// This location was already checked, so replay the earlier outcome.
			if c.isInvalid(info.Location) {
				return crawler.ErrStopRecursion
			}
			if v.noRecursion {
				return crawler.ErrStopRecursion
			}
			return nil
		}

		found := v.checkResource(resource, info)
		if len(found) == 0 {
			if v.noRecursion {
				return crawler.ErrStopRecursion
			}
			return nil
		}

		c.addResourceIssues(info.Location, resource, found)
		// Do not follow links from a resource that could not be fully validated:
		// its links could not be resolved reliably.
		return crawler.ErrStopRecursion
	}
}

type reportCollector struct {
	mu            sync.Mutex
	visited       map[string]struct{}
	invalid       map[string]struct{}
	issueKeys     map[string]struct{}
	issues        []*Issue
	unknownErrors int
}

// beginResource records a visit to a resource location.  It returns false when
// the location has already been visited, so validation is only done once per
// distinct resource.
func (c *reportCollector) beginResource(location string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.visited[location]; ok {
		return false
	}
	c.visited[location] = struct{}{}
	return true
}

func (c *reportCollector) isInvalid(location string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.invalid[location]
	return ok
}

func (c *reportCollector) addResourceIssues(location string, resource crawler.Resource, found []*resourceIssue) {
	id := resourceStringField(resource, "id")
	resourceType := string(resource.Type())
	version := resource.Version()

	issues := []*Issue{}
	for _, item := range found {
		leaves := []*jsonschema.ValidationError{nil}
		if item.schemaErr != nil {
			leaves = schemaLeaves(item.schemaErr)
		}
		for _, leaf := range leaves {
			issue := &Issue{
				Location:    location,
				ID:          id,
				Type:        resourceType,
				STACVersion: version,
				Phase:       item.phase,
				SchemaURL:   item.schemaURL,
				Message:     item.message,
			}
			if leaf != nil {
				issue.Message = leaf.Message
				issue.InstanceLocation = leaf.InstanceLocation
				issue.KeywordLocation = leaf.KeywordLocation
				issue.AbsoluteKeywordLocation = leaf.AbsoluteKeywordLocation
			}
			issues = append(issues, issue)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalid[location] = struct{}{}
	for _, issue := range issues {
		c.addIssueLocked(issue)
	}
}

// handleError is the crawler ErrorHandler used during report generation.
func (c *reportCollector) handleError(err error) error {
	if err == nil {
		return nil
	}
	// The visitor returns ErrStopRecursion to skip the links of an invalid
	// resource; let the crawler handle it.
	if errors.Is(err, crawler.ErrStopRecursion) {
		return err
	}

	var loadErr *crawler.LoadError
	if errors.As(err, &loadErr) {
		phase := PhaseError
		switch loadErr.Phase {
		case crawler.LoadRead:
			phase = PhaseRead
		case crawler.LoadParse:
			phase = PhaseParse
		}
		c.addIssue(&Issue{
			Location: loadErr.Location,
			Phase:    phase,
			Message:  loadErr.Error(),
		})
		return nil
	}

	// Never silently swallow an unexpected error: record it and keep going.
	c.addIssue(&Issue{
		Phase:   PhaseError,
		Message: err.Error(),
	})
	return nil
}

func (c *reportCollector) addIssue(issue *Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.addIssueLocked(issue) {
		return
	}
	if issue.Location != "" {
		c.invalid[issue.Location] = struct{}{}
	} else {
		c.unknownErrors++
	}
}

func (c *reportCollector) addIssueLocked(issue *Issue) bool {
	key := issueKey(issue)
	if _, ok := c.issueKeys[key]; ok {
		return false
	}
	c.issueKeys[key] = struct{}{}
	c.issues = append(c.issues, issue)
	return true
}

func issueKey(issue *Issue) string {
	return string(issue.Phase) + "\x00" +
		issue.Location + "\x00" +
		issue.SchemaURL + "\x00" +
		issue.InstanceLocation + "\x00" +
		issue.KeywordLocation + "\x00" +
		issue.AbsoluteKeywordLocation + "\x00" +
		issue.Message
}

func (c *reportCollector) report() *Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	sort.SliceStable(c.issues, func(i, j int) bool {
		return issueLess(c.issues[i], c.issues[j])
	})

	valid := 0
	for location := range c.visited {
		if _, bad := c.invalid[location]; !bad {
			valid++
		}
	}

	return &Report{
		Valid:   valid,
		Invalid: len(c.invalid) + c.unknownErrors,
		Issues:  c.issues,
	}
}

func issueLess(a, b *Issue) bool {
	fields := []string{
		a.Location, b.Location,
		string(a.Phase), string(b.Phase),
		a.InstanceLocation, b.InstanceLocation,
		a.KeywordLocation, b.KeywordLocation,
		a.AbsoluteKeywordLocation, b.AbsoluteKeywordLocation,
		a.SchemaURL, b.SchemaURL,
		a.Message, b.Message,
	}
	for i := 0; i < len(fields); i += 2 {
		if fields[i] != fields[i+1] {
			return fields[i] < fields[i+1]
		}
	}
	return false
}

// schemaLeaves returns the leaf errors of a (possibly nested) schema
// validation error tree.  The order is the deterministic order produced by
// the JSON Schema validator.
func schemaLeaves(err *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(err.Causes) == 0 {
		return []*jsonschema.ValidationError{err}
	}
	leaves := []*jsonschema.ValidationError{}
	for _, cause := range err.Causes {
		leaves = append(leaves, schemaLeaves(cause)...)
	}
	return leaves
}

func resourceStringField(resource crawler.Resource, key string) string {
	value, ok := resource[key]
	if !ok {
		return ""
	}
	stringValue, ok := value.(string)
	if !ok {
		return ""
	}
	return stringValue
}
