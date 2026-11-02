package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/planetlabs/go-stac/validator"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

var validateCommand = &cli.Command{
	Name:        "validate",
	Usage:       "Validate STAC metadata",
	Description: "Validates that STAC metadata is conforms with the specification.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    flagEntry,
			Usage:   "Path to STAC resource (catalog, collection, or item) to validate",
			EnvVars: []string{toEnvVar(flagEntry)},
		},
		&cli.StringSliceFlag{
			Name:    flagSchema,
			Usage:   "Substitute schema as <original>=<substitute> pairs",
			EnvVars: []string{toEnvVar(flagSchema)},
		},
		&cli.BoolFlag{
			Name:    flagNoRecursion,
			Usage:   "Visit a single resource",
			EnvVars: []string{toEnvVar(flagNoRecursion)},
		},
		&cli.BoolFlag{
			Name:    flagNoFailFast,
			Usage:   "Do not stop at the first invalid resource; continue the crawl and report every problem",
			EnvVars: []string{toEnvVar(flagNoFailFast)},
		},
		&cli.BoolFlag{
			Name:    flagJSON,
			Usage:   "Print the validation report as JSON (implies --" + flagNoFailFast + ")",
			EnvVars: []string{toEnvVar(flagJSON)},
		},
		&cli.GenericFlag{
			Name:  flagLogLevel,
			Usage: fmt.Sprintf("Log level (%s)", strings.Join(logLevelValues, ", ")),
			Value: &Enum{
				Values:  logLevelValues,
				Default: zap.ErrorLevel.String(),
			},
			EnvVars: []string{toEnvVar(flagLogLevel)},
		},
	},
	Action: func(ctx *cli.Context) error {
		logger, sync, logErr := configureLogger(ctx)
		if logErr != nil {
			return logErr
		}
		defer sync()

		entryPath := ctx.String(flagEntry)
		if entryPath == "" {
			return cli.Exit(fmt.Sprintf("missing --%s", flagEntry), 1)
		}

		schemaMap := map[string]string{}
		for _, pair := range ctx.StringSlice(flagSchema) {
			items := strings.Split(pair, "=")
			if len(items) != 2 {
				return cli.Exit(fmt.Sprintf("invalid --%s value %q", flagSchema, pair), 1)
			}
			schemaMap[items[0]] = items[1]
		}

		v := validator.New(&validator.Options{
			NoRecursion: ctx.Bool(flagNoRecursion),
			SchemaMap:   schemaMap,
			Logger:      logger,
		})

		reportMode := ctx.Bool(flagNoFailFast) || ctx.Bool(flagJSON)
		if !reportMode {
			err := v.Validate(context.Background(), entryPath)
			if err != nil {
				if validationErr, ok := err.(*validator.ValidationError); ok {
					return cli.Exit(fmt.Sprintf("%#v\n", validationErr), 2)
				}
				return cli.Exit(fmt.Sprintf("validation failed: %s\n", err), 3)
			}
			return nil
		}

		report, err := v.ValidateReport(context.Background(), entryPath)
		if err != nil {
			return cli.Exit(fmt.Sprintf("validation failed: %s\n", err), 3)
		}

		if ctx.Bool(flagJSON) {
			if encodeErr := writeJSONReport(os.Stdout, report); encodeErr != nil {
				return cli.Exit(fmt.Sprintf("failed to write report: %s\n", encodeErr), 3)
			}
		} else {
			writeTextReport(os.Stdout, report)
		}

		if report.Invalid > 0 {
			return cli.Exit("", 2)
		}
		return nil
	},
}

func writeJSONReport(w io.Writer, report *validator.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func writeTextReport(w io.Writer, report *validator.Report) {
	currentLocation := ""
	for _, issue := range report.Issues {
		if issue.Location != currentLocation {
			currentLocation = issue.Location
			fmt.Fprintln(w, formatIssueHeader(issue))
		}
		fmt.Fprintf(w, "  [%s] %s\n", issue.Phase, issue.Message)
		if issue.InstanceLocation != "" {
			fmt.Fprintf(w, "      instance: %s\n", issue.InstanceLocation)
		}
		if issue.KeywordLocation != "" {
			fmt.Fprintf(w, "      keyword:  %s\n", issue.KeywordLocation)
		}
		if issue.AbsoluteKeywordLocation != "" {
			fmt.Fprintf(w, "      schema:    %s\n", issue.AbsoluteKeywordLocation)
		}
		if issue.SchemaURL != "" {
			fmt.Fprintf(w, "      schema url: %s\n", issue.SchemaURL)
		}
	}
	fmt.Fprintf(w, "\n%d valid, %d invalid\n", report.Valid, report.Invalid)
}

func formatIssueHeader(issue *validator.Issue) string {
	location := issue.Location
	if location == "" {
		location = "(unknown location)"
	}
	details := []string{}
	if issue.Type != "" {
		details = append(details, issue.Type)
	}
	if issue.ID != "" {
		details = append(details, fmt.Sprintf("id: %s", issue.ID))
	}
	if issue.STACVersion != "" {
		details = append(details, fmt.Sprintf("stac_version: %s", issue.STACVersion))
	}
	if len(details) == 0 {
		return fmt.Sprintf("invalid resource: %s", location)
	}
	return fmt.Sprintf("invalid %s: %s", strings.Join(details, ", "), location)
}
