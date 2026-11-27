// Package terrakubequeue connects reviewed deployment settings to Git and
// Terrakube.
package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue/reconcile"
)

const (
	promotionEnvironmentEnv = "PROMOTION_ENVIRONMENT"
	promotionRepositoryEnv  = "PROMOTION_GITHUB_REPOSITORY"
	baselineSHAEnv          = "TERRAKUBE_BASELINE_SHA"
	apiURLEnv               = "TERRAKUBE_API_URL"
	organizationIDEnv       = "TERRAKUBE_ORGANIZATION_ID"
	workspaceIDEnv          = "TERRAKUBE_WORKSPACE_ID"
	templateIDEnv           = "TERRAKUBE_TEMPLATE_ID"
)

var (
	errWorkspaceName       = errors.New("workspace is not enrolled on the reviewed main branch")
	errPromotionRepository = errors.New(
		promotionRepositoryEnv + " must be a GitHub owner/repository name",
	)
	errBaseline = errors.New(
		baselineSHAEnv + " must be the full initial promotion commit",
	)
)

// Dependencies supply deployment settings, authentication, and polling.
type Dependencies struct {
	Environment func(string) string
	Token       func(context.Context, string) (string, error)
	HTTPClient  *http.Client
	Wait        func(context.Context) error
}

// Service operates on reviewed enrollment from a deployment checkout.
type Service struct {
	deps Dependencies
}

// NewService connects the queue to its runtime dependencies.
//
// Callers supply settings, authentication, HTTP, and polling.
//
//   - Production uses settings and credentials from GitHub Actions.
//   - Tests can supply fixed settings and control when polling continues.
func NewService(deps Dependencies) Service {
	return Service{deps: deps}
}

// Deploy advances one enrolled workspace in merge order.
//
// It uses the reviewed enrollment from the checkout to select the workspace.
// For example, deploy the QA workspace of an enrolled application.
//
//	err := service.Deploy(ctx, "app-qa", "./deployment")
func (service Service) Deploy(ctx context.Context, deployment, checkout string) error {
	entries, err := readTargets(ctx, checkout)
	if err != nil {
		return err
	}

	return service.deploy(ctx, deployment, checkout, entries)
}

// Affected reports whether a commit range changes the selected workspace.
//
// It checks the workspace root, its modules, and referenced files.
//
// For example, check whether a commit range affects the QA workspace.
//
//	changed, err := service.Affected(ctx, checkout, qaRoot, base, head)
func (service Service) Affected(
	ctx context.Context,
	checkout string,
	root string,
	base string,
	head string,
) (bool, error) {
	entries, err := readTargets(ctx, checkout)
	if err != nil {
		return false, err
	}

	for _, entry := range entries {
		if entry.Root == root {
			source := gitSource{Directory: checkout, Root: root, Shared: entry.Shared}

			return source.affected(ctx, base, head)
		}
	}

	return false, errWorkspaceName
}

// Matrix lists enrolled workspaces from the reviewed main branch.
//
// Workflows use this list to select the workspaces for each job.
//
//   - Deployment jobs select enabled workspaces.
//   - Preview jobs also select disabled workspaces.
func (service Service) Matrix(
	ctx context.Context,
	checkout string,
	includeDisabled bool,
) ([]Target, error) {
	entries, err := readTargets(ctx, checkout)
	if err != nil {
		return nil, err
	}

	if includeDisabled {
		return entries, nil
	}

	return slices.DeleteFunc(entries, func(entry Target) bool { return !*entry.Enabled }), nil
}

func (service Service) deploy(
	ctx context.Context,
	deployment string,
	checkout string,
	entries []Target,
) error {
	index := slices.IndexFunc(entries, func(entry Target) bool { return entry.Name == deployment })
	if index < 0 {
		return errDisabled
	}

	selected := entries[index]
	env := service.deps.Environment
	if selected.Enabled == nil || !*selected.Enabled ||
		selected.Environment != env(promotionEnvironmentEnv) {
		return errDisabled
	}

	repository := env(promotionRepositoryEnv)
	if !repoName.MatchString(repository) || !validRepoPath(repository) {
		return errPromotionRepository
	}

	baseline := env(baselineSHAEnv)
	if !isCanonicalCommit(baseline) {
		return errBaseline
	}

	expectedOrigin := githubHTTPSOrigin + repository
	source := gitSource{
		Directory:      checkout,
		Branch:         selected.Branch,
		Root:           selected.Root,
		Shared:         selected.Shared,
		expectedOrigin: expectedOrigin,
		promotionToken: func() string { return env(promotionTokenEnv) },
	}
	api := apiClient{
		BaseURL:      env(apiURLEnv),
		Organization: env(organizationIDEnv),
		Workspace:    env(workspaceIDEnv),
		Template:     env(templateIDEnv),
		Token:        service.deps.Token,
		Client:       service.deps.HTTPClient,
	}
	if err := api.validate(); err != nil {
		return err
	}

	if err := api.verifyWorkspace(ctx, source.Branch, expectedOrigin); err != nil {
		return err
	}

	if err := reconcile.Run(ctx, baseline, source, api, service.deps.Wait); err != nil {
		slog.ErrorContext(ctx, "advance deployment", "workspace", deployment, "err", err)

		return fmt.Errorf("advance deployment %s: %w", deployment, err)
	}

	return nil
}
