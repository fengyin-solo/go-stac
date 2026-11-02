package crawler

// LoadPhase describes the stage at which loading a resource failed.
type LoadPhase string

const (
	// LoadRead means the resource could not be read from the local filesystem
	// or fetched over the network.
	LoadRead LoadPhase = "read"

	// LoadParse means the resource was read but its contents could not
	// be parsed as JSON.
	LoadParse LoadPhase = "parse"
)

// LoadError represents a failure to read or parse a resource.
//
// It allows callers (such as validators collecting all problems during a
// crawl) to distinguish resource access failures from validation failures.
type LoadError struct {
	// Location is the URL or file path of the resource that could not be loaded.
	Location string

	// Phase indicates whether the failure happened while reading or parsing
	// the resource.
	Phase LoadPhase

	// Err is the underlying error.
	Err error
}

func (e *LoadError) Error() string {
	return e.Err.Error()
}

func (e *LoadError) Unwrap() error {
	return e.Err
}
