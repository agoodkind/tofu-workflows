// Package tack publishes verified Tack release pins in Configs.
package tack

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/githubapi"
)

var (
	errPositionalArguments  = errors.New("tack-release does not accept positional arguments")
	errMissingBuildIdentity = errors.New(
		"--artifact requires a positive --build-run and --build-attempt",
	)
	errMissingArtifact = errors.New("build metadata requires --artifact")
)

const publicRequestTimeout = 30 * time.Second

// Run executes the Tack release publisher command.
//
// Run handles interruption and prints the Configs release PR URLs.
//
// The executable can pass the returned status to the operating system.
//
//	os.Exit(Run())
func Run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := publishTackCommand(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("Tack release publication failed", "err", err)

		return 1
	}

	return 0
}

type publishOptions struct {
	artifact   string
	runID      int64
	attempt    int
	slug       string
	reporterID int64
}

func parsePublishOptions(args []string, stderr io.Writer) (publishOptions, error) {
	var options publishOptions
	flags := flag.NewFlagSet("tack-release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(
		&options.artifact,
		"artifact",
		"",
		"Path to the verified workflow artifact ZIP. Omit to reconcile pending pull requests.",
	)
	flags.Int64Var(&options.runID, "build-run", 0, "Tack workflow run ID.")
	flags.IntVar(&options.attempt, "build-attempt", 0, "Tack workflow run attempt.")
	flags.StringVar(
		&options.slug,
		"publisher-slug",
		"",
		"Configs release publisher GitHub App slug.",
	)
	flags.Int64Var(
		&options.reporterID,
		"reporter-app-id",
		0,
		"GitHub App ID required to report Terrakube plan.",
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options, flag.ErrHelp
		}

		slog.Error("Parse publish-tack arguments failed", "err", err)

		return options, fmt.Errorf("parse publish-tack arguments: %w", err)
	}

	if flags.NArg() != 0 {
		return options, errPositionalArguments
	}

	if options.artifact != "" && (options.runID <= 0 || options.attempt <= 0) {
		return options, errMissingBuildIdentity
	}

	if options.artifact == "" && (options.runID != 0 || options.attempt != 0) {
		return options, errMissingArtifact
	}

	return options, nil
}

func publishTackCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parsePublishOptions(args, stderr)
	if err != nil {
		return err
	}

	token := os.Getenv("CONFIGS_PUBLISHER_TOKEN")
	target, err := githubapi.NewClient(token)
	if err != nil {
		slog.Error("Initialize Configs GitHub client failed", "err", err)

		return fmt.Errorf("initialize Configs GitHub client: %w", err)
	}

	source, err := tackSourceClient(options.artifact)
	if err != nil {
		return err
	}

	var file *os.File
	if options.artifact != "" {
		file, err = os.Open(options.artifact)
		if err != nil {
			slog.Error("Open Tack release artifact failed", "err", err)

			return fmt.Errorf("open Tack release artifact: %w", err)
		}
	}

	var artifact io.Reader
	if file != nil {
		artifact = file
	}

	urls, err := PublishTack(
		ctx,
		source,
		target,
		token,
		options.slug,
		options.reporterID,
		artifact,
		options.runID,
		options.attempt,
	)
	if file != nil {
		err = errors.Join(err, file.Close())
	}

	if err != nil {
		slog.Error("Publish Tack release failed", "err", err)

		return fmt.Errorf("publish Tack release: %w", err)
	}

	for _, url := range urls {
		if _, err := fmt.Fprintln(stdout, url); err != nil {
			slog.Error("Write Configs release PR URL failed", "err", err)

			return fmt.Errorf("write Configs release PR URL: %w", err)
		}
	}

	return nil
}

func tackSourceClient(artifactPath string) (*github.Client, error) {
	if artifactPath != "" {
		client, err := githubapi.NewClient(os.Getenv("TACK_BUILD_READ_TOKEN"))
		if err != nil {
			slog.Error("Initialize Tack build client failed", "err", err)

			return nil, fmt.Errorf("initialize Tack build client: %w", err)
		}

		return client, nil
	}

	client, err := github.NewClient(
		github.WithHTTPClient(&http.Client{Timeout: publicRequestTimeout}),
	)
	if err != nil {
		slog.Error("Initialize public Tack read client failed", "err", err)

		return nil, fmt.Errorf("initialize public Tack read client: %w", err)
	}

	return client, nil
}
