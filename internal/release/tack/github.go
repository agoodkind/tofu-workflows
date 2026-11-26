package tack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

var (
	errGitHubGraphQL = errors.New("GitHub GraphQL query failed")
	errPinFile       = errors.New(
		"a Configs pin input must be a regular file",
	)
	errConfigsReleaseBranchRequiresVerified = errors.New(
		"the Configs release branch requires a verified commit from the release publisher",
	)
	errConfigsReleaseCommitLacksBounded = errors.New(
		"the Configs release commit lacks bounded release evidence",
	)
	errGitHubDidNotVerifyConfigs = errors.New(
		"GitHub did not verify the Configs pin commit",
	)
	errMergeableReleasePRHeadDoes = errors.New(
		"the mergeable release PR head does not match its signed release evidence",
	)
	errGitHubDidNotMergeConfigs = errors.New(
		"GitHub did not merge the Configs release PR",
	)
)

const (
	quietGit             = "--quiet"
	releaseBranchPrefix  = "automation/tack-release/"
	commitManifestPrefix = "Tack release evidence: "
	terrakubePlanContext = "Terrakube plan"
	configsRemote        = "https://github.com/agoodkind/configs.git"
)

var gitHubAppSlug = regexp.MustCompile("^[a-z0-9][a-z0-9-]*$")

type publisher struct {
	source        *github.Client
	target        *github.Client
	token         string
	bot           string
	reporterAppID int64
}

func (p publisher) branchSHA(ctx context.Context, branch string) (string, error) {
	ref, response, err := p.target.Git.GetRef(ctx, repoOwner, configsRepo, "heads/"+branch)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return "", nil
	}

	if err != nil {
		slog.Error("Read Configs branch failed", "branch", branch, "err", err)

		return "", fmt.Errorf("read Configs branch %s: %w", branch, err)
	}

	return ref.GetObject().GetSHA(), nil
}

func (p publisher) pinFiles(ctx context.Context, sha string) (map[string]string, error) {
	files := make(map[string]string, len(tackPinPaths))

	for _, path := range tackPinPaths {
		file, _, _, err := p.target.Repositories.GetContents(
			ctx,
			repoOwner,
			configsRepo,
			path,
			&github.RepositoryContentGetOptions{Ref: sha},
		)
		if err != nil {
			slog.Error("Read Configs failed", "err", err)

			return nil, fmt.Errorf("read Configs %s: %w", path, err)
		}

		if file.GetType() != "file" {
			return nil, fmt.Errorf("%w: %s", errPinFile, path)
		}

		files[path], err = file.GetContent()
		if err != nil {
			slog.Error("Decode Configs failed", "err", err)

			return nil, fmt.Errorf("decode Configs %s: %w", path, err)
		}
	}

	return files, nil
}

func (p publisher) trustedCommit(
	ctx context.Context,
	sha string,
) (*github.RepositoryCommit, error) {
	commit, _, err := p.target.Repositories.GetCommit(ctx, repoOwner, configsRepo, sha, nil)
	if err != nil {
		slog.Error("Read Configs release commit failed", "err", err)

		return nil, fmt.Errorf("read Configs release commit: %w", err)
	}

	if commit.GetSHA() != sha ||
		commit.GetAuthor().GetLogin() != p.bot ||
		!commit.GetCommit().GetVerification().GetVerified() ||
		len(commit.Parents) != 1 {
		return nil, errConfigsReleaseBranchRequiresVerified
	}

	return commit, nil
}

func marshalCommitEvidence(manifest release.Manifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		slog.Error("Encode release evidence failed", "err", err)

		return "", fmt.Errorf("encode release evidence: %w", err)
	}

	return commitManifestPrefix + base64.StdEncoding.EncodeToString(data), nil
}

func manifestFromCommit(commit *github.RepositoryCommit) (release.Manifest, error) {
	_, text, found := strings.Cut(commit.GetCommit().GetMessage(), "\n\n"+commitManifestPrefix)
	if !found {
		return release.Manifest{}, errConfigsReleaseCommitLacksBounded
	}

	manifest, err := release.Decode(
		base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(text))),
	)
	if err != nil {
		slog.Error("Decode Configs release evidence failed", "err", err)

		return release.Manifest{}, fmt.Errorf("decode Configs release evidence: %w", err)
	}

	return manifest, nil
}

type fileAddition struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type commitInput struct {
	Branch          map[string]string         `json:"branch"`
	ExpectedHeadOID string                    `json:"expectedHeadOid"`
	FileChanges     map[string][]fileAddition `json:"fileChanges"`
	Message         map[string]string         `json:"message"`
}

func (p publisher) signedCommit(
	ctx context.Context,
	branch string,
	base string,
	files map[string]string,
	manifest release.Manifest,
) (string, error) {
	evidence, err := marshalCommitEvidence(manifest)
	if err != nil {
		return "", err
	}

	additions := make([]fileAddition, 0, len(files))

	for _, path := range tackPinPaths {
		additions = append(
			additions,
			fileAddition{
				Path:     path,
				Contents: base64.StdEncoding.EncodeToString([]byte(files[path])),
			},
		)
	}

	var response struct {
		CreateCommitOnBranch struct {
			Commit struct {
				OID       string `json:"oid"`
				Signature struct {
					IsValid bool `json:"isValid"`
				} `json:"signature"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}
	payload, err := json.Marshal(commitInput{
		Branch: map[string]string{
			"repositoryNameWithOwner": repoOwner + "/" + configsRepo,
			"branchName":              branch,
		},
		ExpectedHeadOID: base,
		FileChanges:     map[string][]fileAddition{"additions": additions},
		Message: map[string]string{
			"headline": "Pin Tack QA and production to " + manifest.SourceSHA[:8],
			"body":     evidence,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode signed commit request: %w", err)
	}

	responseData, err := p.graphQL(
		ctx,
		`mutation($input: CreateCommitOnBranchInput!) {
		createCommitOnBranch(input: $input) { commit { oid signature { isValid } } }
	}`,
		payload,
	)
	if err != nil {
		return "", fmt.Errorf("create signed Configs pin commit: %w", err)
	}

	if err := json.Unmarshal(responseData, &response); err != nil {
		slog.Error("Decode GitHub commit response failed", "err", err)

		return "", fmt.Errorf("decode GitHub commit response: %w", err)
	}

	commit := response.CreateCommitOnBranch.Commit
	if commit.OID == "" || !commit.Signature.IsValid {
		return "", errGitHubDidNotVerifyConfigs
	}

	return commit.OID, nil
}

type graphQLRequest struct {
	Query     string                     `json:"query"`
	Variables map[string]json.RawMessage `json:"variables"`
}

func (p publisher) graphQL(
	ctx context.Context,
	query string,
	inputValue json.RawMessage,
) (json.RawMessage, error) {
	request, err := p.target.NewRequest(
		ctx,
		http.MethodPost,
		"graphql",
		graphQLRequest{
			Query:     query,
			Variables: map[string]json.RawMessage{"input": inputValue},
		},
	)
	if err != nil {
		slog.Error("Create GitHub GraphQL request failed", "err", err)

		return nil, fmt.Errorf("create GitHub GraphQL request: %w", err)
	}

	var response struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := p.target.Do(request, &response); err != nil {
		slog.Error("Send GitHub GraphQL request failed", "err", err)

		return nil, fmt.Errorf("send GitHub GraphQL request: %w", err)
	}

	if len(response.Errors) != 0 {
		return nil, fmt.Errorf("%w: %s", errGitHubGraphQL, response.Errors[0].Message)
	}

	return response.Data, nil
}

func (p publisher) updateBranch(
	ctx context.Context,
	candidateBranch string,
	commit string,
	branch string,
	expected string,
) error {
	directory, err := os.MkdirTemp("", "tack-release-git-")
	if err != nil {
		slog.Error("Create temporary Git directory failed", "err", err)

		return fmt.Errorf("create temporary Git directory: %w", err)
	}

	writeErr := pushReleaseRef(
		ctx,
		directory,
		configsRemote,
		p.token,
		candidateBranch,
		commit,
		branch,
		expected,
	)

	return errors.Join(writeErr, os.RemoveAll(directory))
}

func pushReleaseRef(
	ctx context.Context,
	directory string,
	remote string,
	token string,
	candidateBranch string,
	commit string,
	branch string,
	expected string,
) error {
	candidate := "refs/heads/" + candidateBranch
	target := "refs/heads/" + branch
	authorization := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))

	for _, args := range [][]string{
		{"init", "--bare", quietGit},
		{"fetch", quietGit, "--no-tags", "--depth=1", remote, candidate + ":" + candidate},
		{
			pushEvent,
			quietGit,
			"--force-with-lease=" + target + ":" + expected,
			remote,
			commit + ":" + target,
		},
	} {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = directory
		command.Env = append(
			os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+authorization,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			slog.Error("Git failed", "err", err)

			return fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(output))
		}
	}

	return nil
}

func (p publisher) readyAutoMerge(ctx context.Context, pullRequest *github.PullRequest) error {
	repository, _, err := p.target.Repositories.Get(ctx, repoOwner, configsRepo)
	if err != nil {
		slog.Error("Read Configs auto-merge setting failed", "err", err)

		return fmt.Errorf("read Configs auto-merge setting: %w", err)
	}

	if !repository.GetAllowAutoMerge() {
		slog.Info("Configs has not enabled native auto-merge")

		return nil
	}

	request, err := p.target.NewRequest(
		ctx,
		http.MethodGet,
		"repos/"+repoOwner+"/"+configsRepo+"/rules/branches/"+mainBranch,
		nil,
	)
	if err != nil {
		slog.Error("Create Configs rules request failed", "err", err)

		return fmt.Errorf("create Configs rules request: %w", err)
	}

	var rules []struct {
		Type       string
		Parameters struct {
			Required []struct {
				Context       string
				IntegrationID int64 `json:"integration_id"`
			} `json:"required_status_checks"`
		}
	}
	if _, err := p.target.Do(request, &rules); err != nil {
		slog.Error("Read Configs merge rules failed", "err", err)

		return fmt.Errorf("read Configs merge rules: %w", err)
	}

	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}

		for _, check := range rule.Parameters.Required {
			if check.Context == terrakubePlanContext && check.IntegrationID == p.reporterAppID {
				return p.enableAutoMerge(ctx, pullRequest)
			}
		}
	}

	slog.Info(
		"Configs has not required Terrakube plan from the configured reporter",
		"app_id",
		p.reporterAppID,
	)

	return nil
}

func (p publisher) enableAutoMerge(ctx context.Context, pullRequest *github.PullRequest) error {
	payload, err := json.Marshal(map[string]string{
		"pullRequestId": pullRequest.GetNodeID(),
		"mergeMethod":   "MERGE",
	})
	if err != nil {
		slog.Error("Encode GitHub auto-merge request failed", "err", err)

		return fmt.Errorf("encode GitHub auto-merge request: %w", err)
	}

	_, err = p.graphQL(
		ctx,
		`mutation($input: EnablePullRequestAutoMergeInput!) {
		enablePullRequestAutoMerge(input: $input) { pullRequest { id } }
	}`,
		payload,
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
	if refreshErr != nil {
		return fmt.Errorf("enable Configs auto-merge: %w", errors.Join(err, refreshErr))
	}

	if current.AutoMerge != nil || current.GetMerged() {
		return nil
	}

	if current.GetMergeableState() == "clean" {
		return p.mergeReadyRelease(ctx, current)
	}

	slog.Error("Enable Configs auto-merge failed", "err", err)

	return fmt.Errorf("enable Configs auto-merge: %w", err)
}

func (p publisher) mergeReadyRelease(ctx context.Context, pullRequest *github.PullRequest) error {
	sha := pullRequest.GetHead().GetSHA()
	commit, err := p.trustedCommit(ctx, sha)
	if err != nil {
		return err
	}

	manifest, err := manifestFromCommit(commit)
	if err != nil {
		return err
	}

	if pullRequest.GetHead().GetRef() != releaseBranchPrefix+manifest.SourceSHA {
		return errMergeableReleasePRHeadDoes
	}

	_, _, current, err := p.configsBase(ctx)
	if err != nil {
		return err
	}

	fresh, err := p.eligible(ctx, current, manifest.SourceSHA)
	if err != nil {
		return err
	}

	if !fresh {
		return p.closeRelease(ctx, pullRequest)
	}

	result, _, err := p.target.PullRequests.Merge(
		ctx,
		repoOwner,
		configsRepo,
		pullRequest.GetNumber(),
		"",
		&github.PullRequestOptions{
			SHA:         sha,
			MergeMethod: "merge",
		},
	)
	if err != nil {
		slog.Error("Merge Configs release after the required checks failed", "err", err)

		return fmt.Errorf("merge Configs release after the required checks: %w", err)
	}

	if !result.GetMerged() {
		return errGitHubDidNotMergeConfigs
	}

	return nil
}
