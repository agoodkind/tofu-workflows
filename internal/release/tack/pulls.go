package tack

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/input"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

var (
	errConfigsReleaseBranchIsAlready = errors.New(
		"the Configs release branch is already associated with a different author",
	)
	errExistingConfigsReleasePRBe = errors.New(
		"the existing Configs release PR must be ready for review",
	)
	errGitHubDidNotAttributeConfigs = errors.New(
		"the GitHub did not attribute the Configs release PR to the release publisher",
	)
)

func (p publisher) isReleasePR(pullRequest *github.PullRequest) bool {
	source, found := strings.CutPrefix(pullRequest.GetHead().GetRef(), releaseBranchPrefix)

	return found && input.Commit(source) &&
		pullRequest.GetHead().GetRepo().GetFullName() == repoOwner+"/"+configsRepo &&
		pullRequest.GetBase().GetRef() == mainBranch &&
		pullRequest.GetUser().GetLogin() == p.bot
}

func (p publisher) releasePR(ctx context.Context, source string) (*github.PullRequest, error) {
	options := &github.PullRequestListOptions{
		State: "all",
		Head:  repoOwner + ":" + releaseBranchPrefix + source,
		Base:  mainBranch,
	}

	options.PerPage = pageSize
	for pullRequest, err := range p.target.PullRequests.ListIter(
		ctx,
		repoOwner,
		configsRepo,
		options,
	) {
		if err != nil {
			slog.Error("List matching Configs release PRs failed", "err", err)

			return nil, fmt.Errorf("list matching Configs release PRs: %w", err)
		}

		if !p.isReleasePR(pullRequest) {
			return nil, errConfigsReleaseBranchIsAlready
		}

		return pullRequest, nil
	}

	return nil, nil
}

func (p publisher) openReleases(ctx context.Context) iter.Seq2[*github.PullRequest, error] {
	return func(yield func(*github.PullRequest, error) bool) {
		options := &github.PullRequestListOptions{
			State: "open",
			Base:  mainBranch,
		}

		options.PerPage = pageSize
		for pullRequest, err := range p.target.PullRequests.ListIter(
			ctx,
			repoOwner,
			configsRepo,
			options,
		) {
			if err != nil {
				yield(nil, fmt.Errorf("list open Configs release PRs: %w", err))

				return
			}

			if p.isReleasePR(pullRequest) && !yield(pullRequest, nil) {
				return
			}
		}
	}
}

func (p publisher) retainRelease(
	ctx context.Context,
	branch string,
	manifest release.Manifest,
	pullRequest *github.PullRequest,
) (*github.PullRequest, error) {
	if pullRequest == nil {
		created, err := p.openRelease(ctx, branch, manifest)
		if err != nil {
			return nil, err
		}

		pullRequest = created
	}

	if pullRequest.GetState() != "open" {
		return pullRequest, nil
	}

	if pullRequest.GetDraft() {
		return nil, errExistingConfigsReleasePRBe
	}

	if err := p.closeSuperseded(ctx, manifest.SourceSHA); err != nil {
		return nil, err
	}

	fresh, err := p.stillEligible(ctx, manifest.SourceSHA)
	if err != nil {
		return nil, err
	}

	if !fresh {
		return nil, p.closeRelease(ctx, pullRequest)
	}

	if err := p.readyAutoMerge(ctx, pullRequest); err != nil {
		return nil, err
	}

	return pullRequest, nil
}

func (p publisher) stillEligible(ctx context.Context, source string) (bool, error) {
	_, _, current, err := p.configsBase(ctx)
	if err != nil {
		return false, err
	}

	return p.eligible(ctx, current, source)
}

func (p publisher) openRelease(
	ctx context.Context,
	branch string,
	manifest release.Manifest,
) (*github.PullRequest, error) {
	body := fmt.Sprintf(
		"Pin Tack QA and production to %s.\n\n[Successful Tack image build]"+
			"(https://github.com/%s/%s/actions/runs/%d/attempts/%d)",
		manifest.SourceSHA,
		repoOwner,
		tackRepo,
		manifest.BuildRunID,
		manifest.BuildAttempt,
	)
	newPR := github.CreatePullRequest{
		Title: new("Pin Tack QA and production to " + manifest.SourceSHA[:8]),
		Head:  branch,
		Base:  mainBranch,
		Body:  new(body),
		Draft: new(false),
	}
	pullRequest, response, err := p.target.PullRequests.Create(ctx, repoOwner, configsRepo, newPR)
	if err == nil {
		if !p.isReleasePR(pullRequest) {
			return nil, errGitHubDidNotAttributeConfigs
		}

		return pullRequest, nil
	}

	if response != nil && response.StatusCode == http.StatusUnprocessableEntity {
		existing, lookupErr := p.releasePR(ctx, manifest.SourceSHA)
		if lookupErr == nil && existing != nil {
			return existing, nil
		}

		err = errors.Join(err, lookupErr)
	}

	slog.Error("Open Configs release PR failed", "err", err)

	return nil, fmt.Errorf("open Configs release PR: %w", err)
}

func (p publisher) closeSuperseded(ctx context.Context, source string) error {
	for pullRequest, err := range p.openReleases(ctx) {
		if err != nil {
			return err
		}

		other := strings.TrimPrefix(pullRequest.GetHead().GetRef(), releaseBranchPrefix)
		order, err := tackOrder(ctx, p.source, other, source)
		if err != nil {
			return err
		}

		if order == commitAhead {
			if err := p.closeRelease(ctx, pullRequest); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p publisher) closeRelease(ctx context.Context, pullRequest *github.PullRequest) error {
	_, _, err := p.target.PullRequests.Edit(
		ctx,
		repoOwner,
		configsRepo,
		pullRequest.GetNumber(),
		&github.PullRequest{
			State: new("closed"),
		},
	)
	if err == nil {
		return nil
	}

	current, _, refreshErr := p.target.PullRequests.Get(
		ctx,
		repoOwner,
		configsRepo,
		pullRequest.GetNumber(),
	)
	if refreshErr == nil && current.GetState() == "closed" {
		return nil
	}

	slog.Error(
		"Close superseded Configs release PR failed",
		"number",
		pullRequest.GetNumber(),
		"err",
		err,
	)

	return fmt.Errorf(
		"close superseded Configs release PR #%d: %w",
		pullRequest.GetNumber(),
		errors.Join(err, refreshErr),
	)
}
