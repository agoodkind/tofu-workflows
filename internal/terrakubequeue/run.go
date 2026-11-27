package terrakubequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sethvargo/go-githubactions"
)

const (
	promotionEnvironmentEnv              = "PROMOTION_ENVIRONMENT"
	promotionRepositoryEnv               = "PROMOTION_GITHUB_REPOSITORY"
	baselineSHAEnv                       = "TERRAKUBE_BASELINE_SHA"
	apiURLEnv                            = "TERRAKUBE_API_URL"
	organizationIDEnv                    = "TERRAKUBE_ORGANIZATION_ID"
	workspaceIDEnv                       = "TERRAKUBE_WORKSPACE_ID"
	templateIDEnv                        = "TERRAKUBE_TEMPLATE_ID"
	matrixDeploy                         = "deploy"
	matrixPreview                        = "preview"
	matrixMode              queueCommand = "matrix"
	affectedMode            queueCommand = "affected"
)

const (
	jobPollInterval       = 15 * time.Second
	argumentCount         = 3
	affectedArgumentCount = 6
	matrixArgumentCount   = 4

	requestTimeout = 30 * time.Second
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
	if len(args) < argumentCount {
		return errUsage
	}

	entries, err := readTargets(ctx, args[2])
	if err != nil {
		return err
	}

	switch queueCommand(args[1]) {
	case matrixMode:
		return matrixCommand(args, entries)
	case affectedMode:
		return affectedCommand(ctx, args, entries)
	}

	return runDeployment(ctx, args, entries)
}

func runDeployment(ctx context.Context, args []string, entries []target) error {
	if len(args) != argumentCount {
		return errUsage
	}

	var selected target

	for _, entry := range entries {
		if entry.Name == args[1] {
			selected = entry

			break
		}
	}

	if selected.Enabled == nil || !*selected.Enabled ||
		selected.Environment != os.Getenv(promotionEnvironmentEnv) {
		return errDisabled
	}

	repository := os.Getenv(promotionRepositoryEnv)
	if !repoName.MatchString(repository) || !validRepoPath(repository) {
		return errPromotionRepository
	}

	expectedOrigin := githubHTTPSOrigin + repository

	baseline := os.Getenv(baselineSHAEnv)
	if !isCanonicalCommit(baseline) {
		return errBaseline
	}

	source := gitSource{
		Directory:      args[2],
		Branch:         selected.Branch,
		Root:           selected.Root,
		Shared:         selected.Shared,
		expectedOrigin: expectedOrigin,
	}
	client := &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	api := apiClient{
		BaseURL:      os.Getenv(apiURLEnv),
		Organization: os.Getenv(organizationIDEnv),
		Workspace:    os.Getenv(workspaceIDEnv),
		Template:     os.Getenv(templateIDEnv),
		Token:        githubactions.GetIDToken,
		Client:       client,
	}
	if err := api.validate(); err != nil {
		return err
	}

	if err := api.verifyWorkspace(ctx, source.Branch, expectedOrigin); err != nil {
		return err
	}

	return drain(ctx, baseline, source, api, waitForJob)
}

var (
	errAffectedUsage = errors.New(
		"usage: terrakube-queue affected CONFIGS_CHECKOUT ROOT BASE_SHA HEAD_SHA",
	)
	errUsage               = errors.New("usage: terrakube-queue DEPLOYMENT CONFIGS_CHECKOUT")
	errWorkspaceName       = errors.New("workspace is not enrolled in reviewed Configs main")
	errPromotionRepository = errors.New(
		promotionRepositoryEnv + " must be a GitHub owner/repository name",
	)
	errBaseline = errors.New(
		baselineSHAEnv + " must be the full initial promotion commit",
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

func affectedCommand(ctx context.Context, args []string, entries []target) error {
	if len(args) != affectedArgumentCount {
		return errAffectedUsage
	}

	for _, entry := range entries {
		if entry.Root == args[3] {
			source := gitSource{Directory: args[2], Root: entry.Root, Shared: entry.Shared}
			changed, err := source.affected(ctx, args[4], args[5])
			if err != nil {
				return err
			}

			fmt.Fprintln(os.Stdout, changed)

			return nil
		}
	}

	return errWorkspaceName
}

func matrixCommand(args []string, entries []target) error {
	if len(args) != matrixArgumentCount || (args[3] != matrixDeploy && args[3] != matrixPreview) {
		return errUsage
	}

	selected := []target{}

	for _, entry := range entries {
		if args[3] == matrixPreview || *entry.Enabled {
			selected = append(selected, entry)
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(selected); err != nil {
		slog.Error("write target matrix", "err", err)

		return fmt.Errorf("write enrollment matrix: %w", err)
	}

	return nil
}

type queueCommand string
