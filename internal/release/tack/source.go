package tack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

var (
	errReleaseInputDoesNotMatch = errors.New(
		"release input does not match a successful Tack main push build",
	)
	errReleaseCandidateIsOutsideTack = errors.New("release candidate is outside Tack main history")
)

type commitOrder string

const (
	commitAhead         commitOrder = "ahead"
	commitBehind        commitOrder = "behind"
	commitIdentical     commitOrder = "identical"
	pushEvent                       = "push"
	pageSize                        = 100
	repoOwner                       = "agoodkind"
	tackRepo                        = "tack"
	configsRepo                     = "configs"
	mainBranch                      = "main"
	tackBuildWorkflowID             = 285032884
	tackBuildWorkflow               = ".github/workflows/build-push.yml"
)

func successfulTackBuild(run *github.WorkflowRun) bool {
	return run.GetWorkflowID() == tackBuildWorkflowID &&
		run.GetPath() == tackBuildWorkflow &&
		run.GetEvent() == pushEvent &&
		run.GetHeadBranch() == mainBranch &&
		run.GetHeadRepository().GetFullName() == repoOwner+"/"+tackRepo &&
		run.GetStatus() == "completed" &&
		run.GetConclusion() == "success"
}

func verifyTackBuild(ctx context.Context, source *github.Client, manifest release.Manifest) error {
	run, _, err := source.Actions.GetWorkflowRunAttempt(
		ctx,
		repoOwner,
		tackRepo,
		manifest.BuildRunID,
		manifest.BuildAttempt,
		nil,
	)
	if err != nil {
		slog.Error("Read Tack build attempt failed", "err", err)

		return fmt.Errorf("read Tack build attempt: %w", err)
	}

	if !successfulTackBuild(run) ||
		run.GetID() != manifest.BuildRunID ||
		run.GetRunAttempt() != manifest.BuildAttempt ||
		run.GetHeadSHA() != manifest.SourceSHA {
		return errReleaseInputDoesNotMatch
	}

	return nil
}

func tackOrder(
	ctx context.Context,
	source *github.Client,
	base string,
	candidate string,
) (commitOrder, error) {
	if base == candidate {
		return commitIdentical, nil
	}

	comparison, _, err := source.Repositories.CompareCommits(
		ctx,
		repoOwner,
		tackRepo,
		base,
		candidate,
		&github.ListOptions{PerPage: 1},
	)
	if err != nil {
		slog.Error("Compare Tack commits failed", "err", err)

		return "", fmt.Errorf("compare Tack commits: %w", err)
	}

	return commitOrder(comparison.GetStatus()), nil
}

func newerSuccessfulTackBuild(
	ctx context.Context,
	source *github.Client,
	candidate string,
) (bool, error) {
	options := &github.ListOptions{PerPage: pageSize}

	for {
		comparison, response, err := source.Repositories.CompareCommits(
			ctx,
			repoOwner,
			tackRepo,
			candidate,
			mainBranch,
			options,
		)
		if err != nil {
			slog.Error("Compare release candidate with Tack main failed", "err", err)

			return false, fmt.Errorf("compare release candidate with Tack main: %w", err)
		}

		order := commitOrder(comparison.GetStatus())
		if order == commitIdentical {
			return false, nil
		}

		if order != commitAhead {
			return false, errReleaseCandidateIsOutsideTack
		}

		for _, commit := range comparison.Commits {
			successful, err := hasSuccessfulBuild(ctx, source, commit.GetSHA())
			if err != nil || successful {
				return successful, err
			}
		}

		if response.NextPage == 0 {
			return false, nil
		}

		options.Page = response.NextPage
	}
}

func hasSuccessfulBuild(ctx context.Context, source *github.Client, commit string) (bool, error) {
	options := &github.ListWorkflowRunsOptions{
		Branch:  mainBranch,
		Event:   pushEvent,
		Status:  "success",
		HeadSHA: commit,
	}

	options.PerPage = pageSize
	for run, err := range source.Actions.ListWorkflowRunsByIDIter(
		ctx,
		repoOwner,
		tackRepo,
		tackBuildWorkflowID,
		options,
	) {
		if err != nil {
			slog.Error("Read Tack build history failed", "err", err)

			return false, fmt.Errorf("read Tack build history: %w", err)
		}

		if successfulTackBuild(run) && run.GetHeadSHA() == commit {
			return true, nil
		}
	}

	return false, nil
}
