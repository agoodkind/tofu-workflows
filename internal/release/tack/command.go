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
	"path/filepath"
	"syscall"
	"time"

	"github.com/agoodkind/tofu-workflows/internal/githubapi"
	"github.com/google/go-github/v93/github"
)

var (
	errPublisherEventPath  = errors.New("set GITHUB_EVENT_PATH to the absolute event file path")
	errPositionalArguments = errors.New("tack-release does not accept positional arguments")
	errPublisherEvent      = errors.New("use a Configs main push or a Tack release dispatch")
)

const (
	publicRequestTimeout = 30 * time.Second
	dispatchEvent        = "repository_dispatch"
)

type publishOptions struct {
	slug       string
	reporterID int64
}

// Run executes the Tack release publisher command.
//
// Run handles interruption and prints the Configs release PR URLs.
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

func parsePublishOptions(args []string, stderr io.Writer) (publishOptions, error) {
	var options publishOptions
	flags := flag.NewFlagSet("tack-release", flag.ContinueOnError)
	flags.SetOutput(stderr)
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

		slog.Error("Parse tack-release arguments failed", "err", err)

		return options, fmt.Errorf("parse tack-release arguments: %w", err)
	}

	if flags.NArg() != 0 {
		return options, errPositionalArguments
	}

	return options, nil
}

func publishTackCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parsePublishOptions(args, stderr)
	if err != nil {
		return err
	}

	event, err := readPublisherEvent(options.slug)
	if err != nil {
		return err
	}

	token := os.Getenv("CONFIGS_PUBLISHER_TOKEN")
	target, err := githubapi.NewClient(token)
	if err != nil {
		slog.Error("Initialize Configs GitHub client failed", "err", err)

		return fmt.Errorf("initialize Configs GitHub client: %w", err)
	}

	source, err := github.NewClient(
		github.WithHTTPClient(&http.Client{Timeout: publicRequestTimeout}),
	)
	if err != nil {
		slog.Error("Initialize public Tack client failed", "err", err)

		return fmt.Errorf("initialize public Tack client: %w", err)
	}

	urls, err := publishTack(ctx, source, target, token, options.slug, options.reporterID, event)
	if err != nil {
		return err
	}

	for _, url := range urls {
		if _, err := fmt.Fprintln(stdout, url); err != nil {
			slog.Error("Write Configs release PR URL failed", "err", err)

			return fmt.Errorf("write Configs release PR URL: %w", err)
		}
	}

	return nil
}

func readPublisherEvent(slug string) (*releaseDispatch, error) {
	if os.Getenv("GITHUB_REPOSITORY") != repoOwner+"/"+configsRepo ||
		os.Getenv("GITHUB_REF") != "refs/heads/"+mainBranch {
		return nil, errPublisherEvent
	}

	switch os.Getenv("GITHUB_EVENT_NAME") {
	case pushEvent:
		return nil, nil
	case dispatchEvent:
		return readDispatchFile(os.Getenv("GITHUB_EVENT_PATH"), publisherBot(slug))
	default:
		return nil, errPublisherEvent
	}
}

func readDispatchFile(path, bot string) (*releaseDispatch, error) {
	if !filepath.IsAbs(path) {
		return nil, errPublisherEventPath
	}

	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		slog.Error("Open GitHub event directory failed", "err", err)

		return nil, fmt.Errorf("open GitHub event directory: %w", err)
	}

	event, err := root.Open(filepath.Base(path))
	if err != nil {
		slog.Error("Open GitHub event failed", "err", err)

		return nil, errors.Join(fmt.Errorf("open GitHub event: %w", err), root.Close())
	}

	dispatch, decodeErr := decodeDispatch(event, bot)

	return dispatch, errors.Join(decodeErr, event.Close(), root.Close())
}
