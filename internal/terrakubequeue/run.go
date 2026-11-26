package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"time"
)

const (
	argumentCount         = 3
	affectedArgumentCount = 6
	qaRoot                = "opentofu/suburban/tack-qa"
	prodRoot              = "opentofu/vault/tack-prod"
	requestTimeout        = 30 * time.Second
)

// run reconciles one Configs workspace with its protected branch.
//
// It reads deployment metadata from the GitHub environment. The checkout
// must contain the full main history and the workspace promotion branch.
//
// Example:
//
//	terrakube-queue tack-qa /workspace/configs
func run(ctx context.Context, args []string) error {
	if len(args) > 1 && args[1] == "affected" {
		if len(args) != affectedArgumentCount {
			return errAffectedUsage
		}

		root := map[string]string{qaRoot: qaRoot, prodRoot: prodRoot}[args[3]]
		if root == "" {
			return errWorkspaceName
		}

		changed, err := Affected(ctx, args[2], root, args[4], args[5])
		if err != nil {
			return err
		}

		fmt.Fprintln(os.Stdout, changed)

		return nil
	}

	if len(args) != argumentCount {
		return errUsage
	}

	workspace := args[1]
	roots := map[string]string{
		"tack-qa":   qaRoot,
		"tack-prod": prodRoot,
	}
	root, ok := roots[workspace]
	if !ok {
		return errWorkspaceName
	}

	baseline := os.Getenv("TERRAKUBE_BASELINE_SHA")
	if !revision.MatchString(baseline) {
		return errBaseline
	}

	source := gitSource{
		Directory: args[2],
		Branch:    "terrakube/" + path.Base(root),
		Root:      root,
	}
	client := &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	api := apiClient{
		BaseURL:      os.Getenv("TERRAKUBE_API_URL"),
		Organization: os.Getenv("TERRAKUBE_ORGANIZATION_ID"),
		Workspace:    os.Getenv("TERRAKUBE_WORKSPACE_ID"),
		Template:     os.Getenv("TERRAKUBE_TEMPLATE_ID"),
		Token:        os.Getenv("TERRAKUBE_TOKEN"),
		Client:       client,
	}
	if err := api.validate(); err != nil {
		return err
	}

	if err := api.verifyWorkspace(ctx, source.Branch); err != nil {
		return err
	}

	return drain(ctx, baseline, source, api, waitForJob)
}

var (
	errAffectedUsage = errors.New(
		"usage: terrakube-queue affected CONFIGS_CHECKOUT ROOT BASE_SHA HEAD_SHA",
	)
	errUsage         = errors.New("usage: terrakube-queue tack-qa|tack-prod CONFIGS_CHECKOUT")
	errWorkspaceName = errors.New("select tack-qa or tack-prod")
	errBaseline      = errors.New(
		"TERRAKUBE_BASELINE_SHA must be the full initial promotion commit",
	)
)

// Main runs the queue command and returns its process status.
//
// A failed reconciliation writes its diagnostic to the runner log.
//
// Example:
//
//	os.Exit(terrakubequeue.Main())
func Main() int {
	if err := run(context.Background(), os.Args); err != nil {
		slog.Error("Configs queue failed", "error", err)

		return 1
	}

	return 0
}

const jobPollInterval = 15 * time.Second

func waitForJob(ctx context.Context) error {
	timer := time.NewTimer(jobPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		slog.WarnContext(ctx, "queue worker stopped polling", "err", ctx.Err())

		return fmt.Errorf("poll Terrakube job: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func drain(
	ctx context.Context,
	baseline string,
	source sourceHistory,
	jobs jobService,
	wait func(context.Context) error,
) error {
	for {
		pending, err := wake(ctx, baseline, source, jobs)
		if err != nil || !pending {
			return err
		}

		if err := wait(ctx); err != nil {
			return err
		}
	}
}
