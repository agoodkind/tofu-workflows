package tack

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

var (
	errConfigsPublisherTokenAppSlug = errors.New(
		"the Configs publisher token, App slug, and Terrakube reporter App ID are required",
	)
	errReleasePRBranchDoesNot = errors.New(
		"the release PR branch does not match its signed release evidence",
	)
	errReleasePRHeadChangedDuring = errors.New(
		"the release PR head changed during publication",
	)
	errReleaseBranchContainsEvidenceDifferent = errors.New(
		"the release branch contains evidence for a different Tack commit",
	)
	errConfigsMainIsMissing          = errors.New("the Configs main is missing")
	errConfigsQAProductionSelectSame = errors.New(
		"the Configs QA and production must select the same Tack release before automatic pinning",
	)
	errCandidateTackReleaseDivergesFrom = errors.New(
		"the candidate Tack release diverges from the active Configs pin",
	)
	errConfigsMainChangedWhilePreparing = errors.New(
		"the Configs main changed while preparing the release PR",
	)
	errNewerSuccessfulTackReleaseSuperseded = errors.New(
		"a newer successful Tack release superseded the candidate during publication",
	)
)

// PublishTack opens or refreshes a ready Configs PR from a verified Tack build.
//
// The publisher validates artifacts against the Tack build history and
// reconciles open release PRs when the caller omits the artifact.
//
// To reconcile open release PRs after a Configs main push, omit the artifact.
//
//	urls, err := PublishTack(ctx, source, target, token, slug, appID, nil, 0, 0)
func PublishTack(
	ctx context.Context,
	source *github.Client,
	target *github.Client,
	token string,
	slug string,
	reporterID int64,
	artifact io.Reader,
	runID int64,
	attempt int,
) ([]string, error) {
	if token == "" || !gitHubAppSlug.MatchString(slug) || reporterID <= 0 {
		return nil, errConfigsPublisherTokenAppSlug
	}

	releasePublisher := publisher{
		source:        source,
		target:        target,
		token:         token,
		bot:           slug + "[bot]",
		reporterAppID: reporterID,
	}
	if artifact == nil {
		return releasePublisher.reconcile(ctx)
	}

	manifest, err := release.ReadArtifact(artifact, runID, attempt)
	if err != nil {
		slog.Error("Read Tack build artifact failed", "err", err)

		return nil, fmt.Errorf("read Tack build artifact: %w", err)
	}

	existing, err := releasePublisher.releasePR(ctx, manifest.SourceSHA)
	if err != nil {
		return nil, err
	}

	url, err := releasePublisher.publish(ctx, manifest, existing)
	if err != nil || url == "" {
		return nil, err
	}

	return []string{url}, nil
}

func (p publisher) reconcile(ctx context.Context) ([]string, error) {
	var urls []string

	for pullRequest, err := range p.openReleases(ctx) {
		if err != nil {
			return nil, err
		}

		commit, err := p.trustedCommit(ctx, pullRequest.GetHead().GetSHA())
		if err != nil {
			return nil, err
		}

		manifest, err := manifestFromCommit(commit)
		if err != nil {
			return nil, err
		}

		url, err := p.publish(ctx, manifest, pullRequest)
		if err != nil {
			return nil, err
		}

		if url != "" {
			urls = append(urls, url)
		}
	}

	return urls, nil
}

func (p publisher) publish(
	ctx context.Context,
	manifest release.Manifest,
	existing *github.PullRequest,
) (string, error) {
	if existing != nil && existing.GetState() == "closed" {
		return existing.GetHTMLURL(), nil
	}

	references, err := tackReferences(manifest)
	if err != nil {
		return "", err
	}

	if existing != nil && existing.GetHead().GetRef() != releaseBranchPrefix+references.source {
		return "", errReleasePRBranchDoesNot
	}

	base, files, current, err := p.configsBase(ctx)
	if err != nil {
		return "", err
	}

	if err := verifyTackBuild(ctx, p.source, manifest); err != nil {
		return "", err
	}

	fresh, err := p.eligible(ctx, current, references.source)
	if err != nil {
		return "", err
	}

	if !fresh {
		if existing != nil {
			return "", p.closeRelease(ctx, existing)
		}

		return "", nil
	}

	pullRequest, err := p.publishEligible(ctx, base, files, current, manifest, references, existing)
	if err != nil {
		return "", err
	}

	return pullRequest.GetHTMLURL(), nil
}

func (p publisher) publishEligible(
	ctx context.Context,
	base string,
	files map[string]string,
	current string,
	manifest release.Manifest,
	references tackRelease,
	existing *github.PullRequest,
) (*github.PullRequest, error) {
	branch := releaseBranchPrefix + references.source
	expected, err := p.branchSHA(ctx, branch)
	if err != nil {
		return nil, err
	}

	if existing != nil && expected != existing.GetHead().GetSHA() {
		return nil, errReleasePRHeadChangedDuring
	}

	if expected != "" {
		stored, parent, err := p.storedRelease(ctx, expected, manifest.SourceSHA)
		if err != nil {
			return nil, err
		}

		manifest = stored
		references, err = tackReferences(stored)
		if err != nil {
			return nil, err
		}

		if parent == base {
			return p.retainRelease(ctx, branch, manifest, existing)
		}
	}

	updated, err := updatedTackPins(files, references, current)
	if err != nil {
		return nil, err
	}

	if err := p.prepareBranch(ctx, base, branch, expected, updated, manifest); err != nil {
		return nil, err
	}

	return p.retainRelease(ctx, branch, manifest, existing)
}

func (p publisher) storedRelease(
	ctx context.Context,
	expected string,
	source string,
) (release.Manifest, string, error) {
	commit, err := p.trustedCommit(ctx, expected)
	if err != nil {
		return release.Manifest{}, "", err
	}

	stored, err := manifestFromCommit(commit)
	if err != nil {
		return release.Manifest{}, "", err
	}

	if stored.SourceSHA != source {
		return release.Manifest{}, "", errReleaseBranchContainsEvidenceDifferent
	}

	return stored, commit.Parents[0].GetSHA(), nil
}

func (p publisher) configsBase(ctx context.Context) (string, map[string]string, string, error) {
	base, err := p.branchSHA(ctx, mainBranch)
	if err != nil {
		return "", nil, "", err
	}

	if base == "" {
		return "", nil, "", errConfigsMainIsMissing
	}

	files, err := p.pinFiles(ctx, base)
	if err != nil {
		return "", nil, "", err
	}

	_, qaCommit, err := releasePin(files[qaPath], qaPath)
	if err != nil {
		return "", nil, "", err
	}

	_, prod, err := releasePin(files[prodPath], prodPath)
	if err != nil {
		return "", nil, "", err
	}

	if qaCommit != prod {
		return "", nil, "", errConfigsQAProductionSelectSame
	}

	return base, files, qaCommit, nil
}

func (p publisher) eligible(ctx context.Context, current, candidate string) (bool, error) {
	order, err := tackOrder(ctx, p.source, current, candidate)
	if err != nil {
		return false, err
	}

	switch order {
	case commitAhead:
		newer, err := newerSuccessfulTackBuild(ctx, p.source, candidate)

		return !newer && err == nil, err
	case commitBehind, commitIdentical:
		return false, nil
	default:
		return false, errCandidateTackReleaseDivergesFrom
	}
}

func (p publisher) prepareBranch(
	ctx context.Context,
	base string,
	branch string,
	expected string,
	files map[string]string,
	manifest release.Manifest,
) error {
	candidate := "automation/tack-release-staging/" + strings.ToLower(rand.Text())
	_, _, err := p.target.Git.CreateRef(
		ctx,
		repoOwner,
		configsRepo,
		github.CreateRef{
			Ref: "refs/heads/" + candidate,
			SHA: base,
		},
	)
	if err != nil {
		slog.Error("Create temporary Configs release branch failed", "err", err)

		return fmt.Errorf("create temporary Configs release branch: %w", err)
	}

	updateErr := p.finishBranch(ctx, base, branch, expected, candidate, files, manifest)
	_, cleanupErr := p.target.Git.DeleteRef(
		context.WithoutCancel(ctx),
		repoOwner,
		configsRepo,
		"heads/"+candidate,
	)
	if cleanupErr != nil {
		slog.Error("Delete temporary Configs release branch failed", "err", cleanupErr)

		return errors.Join(
			updateErr,
			fmt.Errorf("delete temporary Configs release branch: %w", cleanupErr),
		)
	}

	return updateErr
}

func (p publisher) finishBranch(
	ctx context.Context,
	base string,
	branch string,
	expected string,
	candidate string,
	files map[string]string,
	manifest release.Manifest,
) error {
	commit, err := p.signedCommit(ctx, candidate, base, files, manifest)
	if err != nil {
		return err
	}

	latest, err := p.branchSHA(ctx, mainBranch)
	if err != nil {
		return err
	}

	if latest != base {
		return errConfigsMainChangedWhilePreparing
	}

	newer, err := newerSuccessfulTackBuild(ctx, p.source, manifest.SourceSHA)
	if err != nil {
		return err
	}

	if newer {
		return errNewerSuccessfulTackReleaseSuperseded
	}

	return p.updateBranch(ctx, candidate, commit, branch, expected)
}
