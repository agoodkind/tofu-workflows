// Command tofu-workflows validates protected consumer and release inputs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/agoodkind/tofu-workflows/internal/config"
	"github.com/agoodkind/tofu-workflows/internal/input"
	"github.com/agoodkind/tofu-workflows/internal/release"
)

type commandName string

const (
	validateCommand    commandName = "validate"
	credentialsCommand commandName = "credentials"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if code != 0 {
		slog.Error("Command failed", "err", fmt.Errorf("exit status %d", code))
	}
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "A command is required. Use validate or credentials.")
		return 2
	}
	var err error
	failure := "Validation"
	switch commandName(args[0]) {
	case validateCommand:
		err = validate(args[1:], stdout, stderr)
	case credentialsCommand:
		failure = "Credentials"
		err = credentialCommand(args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "Unsupported command %q. Use validate or credentials.\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s failed: %v\n", failure, err)
		return 1
	}
	return 0
}

type validateOptions struct {
	checkout      string
	configuration string
	manifest      string
	artifact      string
	runID         int64
	attempt       int
}

func parseOptions(args []string, stderr io.Writer) (validateOptions, error) {
	var options validateOptions
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.checkout, "checkout", "", "Existing checkout directory containing all input files.")
	flags.StringVar(&options.configuration, "config", "", "Deployment set JSON path relative to the checkout.")
	flags.StringVar(&options.manifest, "manifest", "", "Release JSON path relative to the checkout.")
	flags.StringVar(&options.artifact, "artifact", "", "Release ZIP path relative to the checkout.")
	flags.Int64Var(&options.runID, "expected-run-id", 0, "Expected run ID from authenticated GitHub metadata.")
	flags.IntVar(&options.attempt, "expected-attempt", 0, "Expected attempt from authenticated GitHub metadata.")
	if err := flags.Parse(args); err != nil {
		slog.Error("Command arguments are invalid", "err", err)
		return options, fmt.Errorf("parse validate arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return options, errors.New("validate does not accept positional arguments")
	}
	if options.checkout == "" || options.configuration == "" {
		return options, errors.New("--checkout and --config are required")
	}
	if options.manifest != "" && options.artifact != "" {
		return options, errors.New("select either --manifest or --artifact")
	}
	if options.manifest == "" && options.artifact == "" {
		if options.runID != 0 || options.attempt != 0 {
			return options, errors.New("expected run metadata requires a manifest or artifact")
		}
	} else if options.runID <= 0 || options.attempt <= 0 {
		return options, errors.New("--expected-run-id and --expected-attempt are required for release input")
	}
	return options, nil
}

func validate(args []string, stdout, stderr io.Writer) error {
	options, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	configuration, err := input.RegularFile(options.checkout, options.configuration)
	if err != nil {
		slog.Error("Configuration file validation failed", "err", err)
		return fmt.Errorf("resolve configuration: %w", err)
	}
	set, err := config.Load(configuration)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if err := config.ValidateStateOwnership(set, options.checkout); err != nil {
		return fmt.Errorf("validate state ownership: %w", err)
	}
	if options.manifest != "" || options.artifact != "" {
		manifest, err := loadRelease(options)
		if err != nil {
			return err
		}
		if err := validateRelease(manifest, set); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(stdout, "Consumer inputs are valid.")
	if err != nil {
		return fmt.Errorf("write validation result: %w", err)
	}
	return nil
}

func loadRelease(options validateOptions) (release.Manifest, error) {
	if options.manifest != "" {
		filename, err := input.RegularFile(options.checkout, options.manifest)
		if err != nil {
			slog.Error("Release input path is invalid", "err", err)
			return release.Manifest{}, fmt.Errorf("resolve manifest: %w", err)
		}
		manifest, err := release.Load(filename)
		if err != nil {
			return release.Manifest{}, fmt.Errorf("load manifest: %w", err)
		}
		if manifest.BuildRunID != options.runID || manifest.BuildAttempt != options.attempt {
			return release.Manifest{}, errors.New("release manifest does not match the expected build run and attempt")
		}
		return manifest, nil
	}
	filename, err := input.RegularFile(options.checkout, options.artifact)
	if err != nil {
		return release.Manifest{}, fmt.Errorf("resolve artifact: %w", err)
	}
	file, err := os.Open(filename)
	if err != nil {
		return release.Manifest{}, fmt.Errorf("open artifact: %w", err)
	}
	manifest, readErr := release.ReadArtifact(file, options.runID, options.attempt)
	return manifest, errors.Join(readErr, file.Close())
}

func validateRelease(manifest release.Manifest, set config.DeploymentSet) error {
	for name, reference := range manifest.Artifacts {
		repository, exists := set.ArtifactRepositories[name]
		if !exists || !strings.HasPrefix(reference, repository+"@sha256:") {
			return fmt.Errorf("artifact %q does not match a protected repository", name)
		}
	}
	for name := range set.ArtifactRepositories {
		if _, exists := manifest.Artifacts[name]; !exists {
			return fmt.Errorf("manifest artifact %q is required", name)
		}
	}
	for name, value := range manifest.Variables {
		rule, exists := set.Variables[name]
		if !exists {
			return fmt.Errorf("variable %q is not declared in protected configuration", name)
		}
		kind, err := release.ScalarKind(value)
		if err != nil {
			slog.Error("Release scalar is invalid", "variable", name, "err", err)
			return fmt.Errorf("validate scalar: %w", err)
		}
		if kind != rule.Kind {
			return fmt.Errorf("variable %q must have kind %s", name, rule.Kind)
		}
		if len(rule.AllowedValues) != 0 {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return fmt.Errorf("variable %q must be a string", name)
			}
			if !slices.Contains(rule.AllowedValues, text) {
				return fmt.Errorf("variable %q is outside allowed_values", name)
			}
		}
	}
	for name := range set.Variables {
		if _, exists := manifest.Variables[name]; !exists {
			return fmt.Errorf("manifest variable %q is required", name)
		}
	}
	return nil
}
