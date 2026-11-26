package terrakubepreview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agoodkind/tofu-workflows/internal/githubapi"
	"github.com/google/go-github/v93/github"
)

const (
	owner     = "agoodkind"
	repo      = "configs"
	checkName = "Terrakube plan"
)

var (
	errRevision      = errors.New("invalid pull request number or commit")
	errChanged       = errors.New("the pull request repository, base, head, or state changed")
	errURL           = errors.New("configure private HTTPS Terrakube URLs")
	errConfiguration = errors.New("configure preview organization, template, and API token")
	errWorkspace     = errors.New("configure distinct preview workspace IDs")
)

const (
	requestTimeout = 30 * time.Second
	previewTimeout = 40 * time.Minute
)

var revision = regexp.MustCompile("^[0-9a-f]{40}$")

func run(ctx context.Context) error {
	sha := os.Getenv("PREVIEW_HEAD_SHA")
	base := os.Getenv("PREVIEW_BASE_SHA")
	number, err := strconv.Atoi(os.Getenv("PREVIEW_PR_NUMBER"))
	if err != nil || number < 1 || !revision.MatchString(sha) || !revision.MatchString(base) {
		return errRevision
	}

	reader, err := githubapi.NewClient(os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		slog.Error("Could not create the pull request client", "err", err)

		return fmt.Errorf("create pull request client: %w", err)
	}

	reporter, err := githubapi.NewClient(os.Getenv("REPORTER_TOKEN"))
	if err != nil {
		return fmt.Errorf("create reporter client: %w", err)
	}

	return report(ctx, reader, reporter, number, base, sha, planRoots)
}

type reconcile func(context.Context, string, int, string, string, func() error) error

func report(
	ctx context.Context,
	reader *github.Client,
	reporter *github.Client,
	number int,
	base string,
	sha string,
	plans reconcile,
) error {
	current := func() error {
		pull, _, err := reader.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return fmt.Errorf("read pull request: %w", err)
		}

		if pull.GetState() != "open" || pull.GetBase().GetRef() != "main" ||
			pull.GetBase().GetRepo().GetFullName() != owner+"/"+repo ||
			pull.GetHead().GetRepo().GetFullName() != owner+"/"+repo ||
			pull.GetHead().GetSHA() != sha ||
			pull.GetBase().GetSHA() != base {
			return errChanged
		}

		return nil
	}
	if err := current(); err != nil {
		return err
	}

	comparison, _, err := reader.Repositories.CompareCommits(ctx, owner, repo, base, sha, nil)
	if err != nil {
		return fmt.Errorf("compare preview commits: %w", err)
	}

	if comparison.GetMergeBaseCommit().GetSHA() != base {
		return errChanged
	}

	details := os.Getenv("TERRAKUBE_PREVIEW_UI_URL")

	apiURL := os.Getenv("TERRAKUBE_PREVIEW_API_URL")
	for _, raw := range []string{details, apiURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			u.RawQuery != "" ||
			u.Fragment != "" {
			return errURL
		}
	}

	check, _, err := reporter.Checks.CreateCheckRun(
		ctx,
		owner,
		repo,
		github.CreateCheckRunOptions{
			Name:       checkName,
			HeadSHA:    sha,
			Status:     new("in_progress"),
			DetailsURL: &details,
		},
	)
	if err != nil {
		slog.Error("Could not open the GitHub check", "err", err)

		return fmt.Errorf("create plan check: %w", err)
	}

	planContext, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()
	planErr := plans(planContext, apiURL, number, base, sha, current)
	if planErr == nil {
		planErr = current()
	}

	conclusion := "success"
	summary := "Both preview workspaces completed their plans."
	if planErr != nil {
		conclusion = "failure"
		summary = "The preview could not verify a successful plan for this pull request."
	}

	_, _, reportErr := reporter.Checks.UpdateCheckRun(
		ctx,
		owner,
		repo,
		check.GetID(),
		github.UpdateCheckRunOptions{
			Name:       checkName,
			Status:     new("completed"),
			Conclusion: &conclusion,
			Output:     &github.CheckRunOutput{Title: new(checkName), Summary: &summary},
		},
	)

	return errors.Join(planErr, reportErr)
}

func planRoots(
	ctx context.Context,
	apiURL string,
	number int,
	base string,
	sha string,
	current func() error,
) error {
	client := api{
		base:     strings.TrimRight(apiURL, "/"),
		org:      os.Getenv("TERRAKUBE_PREVIEW_ORGANIZATION_ID"),
		template: os.Getenv("TERRAKUBE_PREVIEW_TEMPLATE_ID"),
		token:    os.Getenv("TERRAKUBE_PREVIEW_TOKEN"),
		client: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if !uuid.MatchString(client.org) || !uuid.MatchString(client.template) || client.token == "" {
		return errConfiguration
	}

	if os.Getenv("TERRAKUBE_PREVIEW_QA_ID") == os.Getenv("TERRAKUBE_PREVIEW_PROD_ID") {
		return errWorkspace
	}

	for _, workspace := range []string{
		os.Getenv("TERRAKUBE_PREVIEW_QA_ID"),
		os.Getenv("TERRAKUBE_PREVIEW_PROD_ID"),
	} {
		if !uuid.MatchString(workspace) {
			return errWorkspace
		}

		if err := client.plan(
			ctx,
			workspace,
			sha,
			fmt.Sprintf("configs-pr-%d-%s-%s", number, base, sha),
			current,
			waitForPoll,
		); err != nil {
			return err
		}
	}

	return nil
}

// Main runs the Configs preview and returns its exit status.
//
// A failed plan records the error in the runner log.
//
// Example:
//
//	os.Exit(terrakubepreview.Main())
func Main() int {
	if err := run(context.Background()); err != nil {
		slog.Error("Configs preview failed", "error", err)

		return 1
	}

	return 0
}
