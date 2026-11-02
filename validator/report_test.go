package validator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/planetlabs/go-stac/validator"
)

func findIssue(report *validator.Report, match func(*validator.Issue) bool) *validator.Issue {
	for _, issue := range report.Issues {
		if match(issue) {
			return issue
		}
	}
	return nil
}

func issuesForLocation(report *validator.Report, suffix string) []*validator.Issue {
	issues := []*validator.Issue{}
	for _, issue := range report.Issues {
		if strings.HasSuffix(issue.Location, suffix) {
			issues = append(issues, issue)
		}
	}
	return issues
}

func assertIssuesSorted(t *testing.T, issues []*validator.Issue) {
	t.Helper()
	sorted := sort.SliceIsSorted(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.InstanceLocation != b.InstanceLocation {
			return a.InstanceLocation < b.InstanceLocation
		}
		return a.KeywordLocation < b.KeywordLocation
	})
	if !sorted {
		t.Fatalf("issues are not sorted by location, phase, and schema path: %#v", issues)
	}
}

// TestValidateReportMultipleErrors verifies that several invalid resources in
// one crawl are all reported, that valid and invalid counts are tracked
// separately, and that resource metadata and structured schema paths are
// retained for every issue.
func (s *Suite) TestValidateReportMultipleErrors() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-multi-errors.json")
	s.Require().NoError(err)

	s.Assert().Equal(2, report.Valid, "catalog and the one valid item should count as valid")
	s.Assert().Equal(2, report.Invalid, "both invalid items should count as invalid")
	s.Assert().NotEmpty(report.Issues)
	assertIssuesSorted(s.T(), report.Issues)

	missingID := findIssue(report, func(i *validator.Issue) bool {
		return strings.HasSuffix(i.Location, "/item-missing-id.json") &&
			i.Message == "missing properties: 'id'"
	})
	s.Require().NotNil(missingID, "expected an issue for the item missing its id")
	s.Assert().Equal(validator.PhaseSchema, missingID.Phase)
	s.Assert().Equal("item", missingID.Type)
	s.Assert().Equal("1.0.0", missingID.STACVersion)
	s.Assert().Equal("", missingID.ID, "id should be omitted when the resource has no id")
	s.Assert().Equal("", missingID.InstanceLocation)
	s.Assert().True(strings.HasSuffix(missingID.KeywordLocation, "/required"))
	s.Assert().True(strings.HasPrefix(missingID.AbsoluteKeywordLocation, "https://schemas.stacspec.org/"))
	s.Assert().True(strings.Contains(missingID.SchemaURL, "item-spec/json-schema/item.json"))

	bbox := findIssue(report, func(i *validator.Issue) bool {
		return strings.HasSuffix(i.Location, "/report/item-invalid-bbox.json") &&
			i.InstanceLocation == "/bbox" &&
			strings.Contains(i.Message, "4 items")
	})
	s.Require().NotNil(bbox, "expected a structured bbox schema issue")
	s.Assert().Equal(validator.PhaseSchema, bbox.Phase)
	s.Assert().Equal("invalid-bbox-item", bbox.ID)
	s.Assert().Equal("item", bbox.Type)
	s.Assert().Equal("1.0.0", bbox.STACVersion)
	s.Assert().True(strings.HasSuffix(bbox.KeywordLocation, "/minItems"))
	s.Assert().True(bbox.AbsoluteKeywordLocation != "")
}

// TestValidateReportExtensionFailure verifies that a resource which passes the
// core schema but violates an extension schema is reported under the
// extension phase, with the extension schema URL and precise schema path.
func (s *Suite) TestValidateReportExtensionFailure() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-extension-invalid.json")
	s.Require().NoError(err)

	s.Assert().Equal(1, report.Valid)
	s.Assert().Equal(1, report.Invalid)

	extensionIssues := issuesForLocation(report, "/report/item-eo-invalid.json")
	s.Require().NotEmpty(extensionIssues)
	for _, issue := range extensionIssues {
		s.Assert().Equal(validator.PhaseExtension, issue.Phase)
		s.Assert().Equal("eo-invalid-item", issue.ID)
		s.Assert().Equal("item", issue.Type)
		s.Assert().Equal("1.0.0", issue.STACVersion)
		s.Assert().Equal(
			"https://stac-extensions.github.io/eo/v1.0.0/schema.json",
			issue.SchemaURL,
		)
	}

	cloudCover := findIssue(report, func(i *validator.Issue) bool {
		return i.Phase == validator.PhaseExtension &&
			i.InstanceLocation == "/properties/eo:cloud_cover" &&
			strings.Contains(i.Message, "100")
	})
	s.Require().NotNil(cloudCover, "expected a structured extension issue for eo:cloud_cover")
	s.Assert().True(strings.HasSuffix(cloudCover.KeywordLocation, "/maximum"))
	s.Assert().True(strings.HasPrefix(
		cloudCover.AbsoluteKeywordLocation,
		"https://stac-extensions.github.io/eo/v1.0.0/schema.json#",
	))
}

// TestValidateReportReadFailure verifies that local read failures are reported
// under the read phase (never as schema errors), that metadata that could not
// be loaded is omitted, and that the rest of the crawl continues.
func (s *Suite) TestValidateReportReadFailure() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-missing-item.json")
	s.Require().NoError(err)

	s.Assert().Equal(2, report.Valid, "the crawl should continue to the valid item")
	s.Assert().Equal(1, report.Invalid)

	readIssue := findIssue(report, func(i *validator.Issue) bool {
		return strings.HasSuffix(i.Location, "/report/does-not-exist.json")
	})
	s.Require().NotNil(readIssue)
	s.Assert().Equal(validator.PhaseRead, readIssue.Phase)
	s.Assert().True(strings.HasPrefix(readIssue.Message, "failed to read file"))
	s.Assert().Equal("", readIssue.ID)
	s.Assert().Equal("", readIssue.Type)
	s.Assert().Equal("", readIssue.STACVersion)
	s.Assert().Equal("", readIssue.KeywordLocation)
	s.Assert().Equal("", readIssue.InstanceLocation)

	// A missing entry resource is a read failure too, not a hard error.
	entryReport, entryErr := v.ValidateReport(
		context.Background(),
		"testdata/cases/v1.0.0/report/does-not-exist-entry.json",
	)
	s.Require().NoError(entryErr)
	s.Assert().Equal(0, entryReport.Valid)
	s.Assert().Equal(1, entryReport.Invalid)
	s.Require().Len(entryReport.Issues, 1)
	s.Assert().Equal(validator.PhaseRead, entryReport.Issues[0].Phase)
}

// TestValidateReportParseFailure verifies that invalid JSON is classified as
// a parse failure rather than a schema failure.
func (s *Suite) TestValidateReportParseFailure() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/not-json.json")
	s.Require().NoError(err)

	s.Assert().Equal(0, report.Valid)
	s.Assert().Equal(1, report.Invalid)
	s.Require().Len(report.Issues, 1)

	issue := report.Issues[0]
	s.Assert().Equal(validator.PhaseParse, issue.Phase)
	s.Assert().True(strings.HasPrefix(issue.Message, "failed to parse"))
	s.Assert().Equal("", issue.Type)
}

// TestValidateReportEmptyCatalog verifies counts for a catalog with no linked
// resources.
func (s *Suite) TestValidateReportEmptyCatalog() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/catalog.json")
	s.Require().NoError(err)

	s.Assert().Equal(1, report.Valid)
	s.Assert().Equal(0, report.Invalid)
	s.Assert().NotNil(report.Issues)
	s.Assert().Empty(report.Issues)
}

// TestValidateReportSkipsInvalidResourceLinks verifies that links below an
// invalid resource are not followed, while unrelated branches of the crawl
// continue.
func (s *Suite) TestValidateReportSkipsInvalidResourceLinks() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-invalid-collection.json")
	s.Require().NoError(err)

	s.Assert().Equal(2, report.Valid, "root catalog and sibling item should be valid")
	s.Assert().Equal(1, report.Invalid, "only the invalid collection should be invalid")

	collectionIssues := issuesForLocation(report, "/report/collection-invalid.json")
	s.Require().Len(collectionIssues, 1)
	s.Assert().Equal(validator.PhaseSchema, collectionIssues[0].Phase)
	s.Assert().Equal("collection", collectionIssues[0].Type)
	s.Assert().Equal("invalid-collection", collectionIssues[0].ID)
	s.Assert().True(strings.Contains(collectionIssues[0].Message, "'license'"))

	skipped := issuesForLocation(report, "/report/item-behind-invalid-collection.json")
	s.Assert().Empty(skipped, "links of the invalid collection must not be followed")
}

// TestValidateReportDeterministic verifies that repeated runs produce the same
// issue order and JSON encoding even under concurrent crawling.
func (s *Suite) TestValidateReportDeterministic() {
	var expected []byte
	for run := 0; run < 8; run++ {
		v := validator.New(&validator.Options{Concurrency: 8})
		report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-multi-errors.json")
		s.Require().NoError(err)
		assertIssuesSorted(s.T(), report.Issues)

		encoded, marshalErr := json.Marshal(report)
		s.Require().NoError(marshalErr)
		if expected == nil {
			expected = encoded
			continue
		}
		s.Assert().Equal(string(expected), string(encoded), "report output must be stable across runs")
	}
}

// TestValidateReportDeduplicatesLinks verifies that a resource linked more
// than once is counted once.
func (s *Suite) TestValidateReportDeduplicatesLinks() {
	v := validator.New(&validator.Options{Concurrency: 4})
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/catalog-with-multiple-items.json")
	s.Require().NoError(err)

	s.Assert().Equal(2, report.Valid, "the catalog and the twice-linked item count once each")
	s.Assert().Equal(0, report.Invalid)
	s.Assert().Empty(report.Issues)
}

// TestValidateReportNoRecursion verifies that the no-recursion option is
// honored in report mode.
func (s *Suite) TestValidateReportNoRecursion() {
	v := validator.New(&validator.Options{NoRecursion: true})
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/catalog-with-item.json")
	s.Require().NoError(err)

	s.Assert().Equal(1, report.Valid)
	s.Assert().Equal(0, report.Invalid)
	s.Assert().Empty(report.Issues)
}

// TestValidateReportJSONShape verifies the stable set of JSON fields for schema
// and read problems.
func (s *Suite) TestValidateReportJSONShape() {
	v := validator.New()
	report, err := v.ValidateReport(context.Background(), "testdata/cases/v1.0.0/report/catalog-multi-errors.json")
	s.Require().NoError(err)

	data, marshalErr := json.Marshal(report)
	s.Require().NoError(marshalErr)
	decoded := map[string]interface{}{}
	s.Require().NoError(json.Unmarshal(data, &decoded))

	s.Assert().Contains(decoded, "valid")
	s.Assert().Contains(decoded, "invalid")
	s.Assert().Contains(decoded, "issues")

	first := report.Issues[0]
	issueData, marshalErr := json.Marshal(first)
	s.Require().NoError(marshalErr)
	issueMap := map[string]interface{}{}
	s.Require().NoError(json.Unmarshal(issueData, &issueMap))

	s.Assert().Contains(issueMap, "location")
	s.Assert().Contains(issueMap, "phase")
	s.Assert().Contains(issueMap, "message")
	s.Assert().Contains(issueMap, "type")
	s.Assert().Contains(issueMap, "stac_version")
	s.Assert().Contains(issueMap, "schema_url")
	s.Assert().Contains(issueMap, "keyword_location")
	s.Assert().Contains(issueMap, "absolute_keyword_location")
}

// TestValidateFailFastParity verifies that the default Validate behavior is
// unchanged: it stops at the first invalid resource with a ValidationError.
func (s *Suite) TestValidateFailFastParity() {
	v := validator.New()
	err := v.Validate(context.Background(), "testdata/cases/v1.0.0/report/catalog-multi-errors.json")
	s.Require().Error(err)
	validationErr, ok := err.(*validator.ValidationError)
	s.Require().True(ok, "expected *validator.ValidationError, got %T: %v", err, err)
	s.Assert().True(strings.HasSuffix(fmt.Sprintf("%#v", validationErr), "missing properties: 'id'"))
}
