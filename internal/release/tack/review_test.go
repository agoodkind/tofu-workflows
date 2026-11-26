package tack

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

func TestAutoMergeFailureNeverCallsMergeEndpoint(t *testing.T) {
	pull := reviewReleasePR()
	requests := 0
	client := testGitHubClient(
		t,
		func(response http.ResponseWriter, request *http.Request) {
			requests++
			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/repos/agoodkind/configs/pulls/7":
				encodeResponse(t, response, pull)
			case request.Method == http.MethodPost && request.URL.Path == "/graphql":
				encodeResponse(
					t,
					response,
					map[string]any{
						"errors": []map[string]string{
							{"message": "Pull request is in clean status"},
						},
					},
				)
			default:
				t.Fatalf("Auto-merge failure sent %s %s", request.Method, request.URL.Path)
			}
		},
	)
	publisher := publisher{target: client, bot: publisherBot(mockApp)}
	err := publisher.enableAutoMerge(t.Context(), pull, newTackCommit)
	if !errors.Is(err, errGitHubGraphQL) || requests != 2 {
		t.Fatalf("Auto-merge returned %v after %d requests", err, requests)
	}
}

func TestAutoMergeRejectsHeadChangedAfterRefresh(t *testing.T) {
	pull := reviewReleasePR()
	head := newTackCommit
	enabled := false
	client := testGitHubClient(
		t,
		func(response http.ResponseWriter, request *http.Request) {
			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/repos/agoodkind/configs/pulls/7":
				encodeResponse(t, response, pull)
				head = laterTackCommit
			case request.Method == http.MethodPost && request.URL.Path == "/graphql":
				var mutation struct {
					Variables struct{ Input map[string]string }
				}
				decodeRequest(t, request, &mutation)
				expected := mutation.Variables.Input["expectedHeadOid"]
				if expected != "" && expected != head {
					encodeResponse(
						t,
						response,
						map[string]any{
							"errors": []map[string]string{{"message": "Head SHA changed"}},
						},
					)

					return
				}

				enabled = true
				encodeResponse(t, response, map[string]any{"data": map[string]any{}})
			default:
				t.Fatalf("Head check sent %s %s", request.Method, request.URL.Path)
			}
		},
	)
	publisher := publisher{target: client, bot: publisherBot(mockApp)}
	err := publisher.enableAutoMerge(t.Context(), pull, newTackCommit)
	if !errors.Is(err, errGitHubGraphQL) || enabled {
		t.Fatalf("Changed PR head returned err=%v and enabled=%v", err, enabled)
	}
}

func reviewReleasePR() *github.PullRequest {
	return &github.PullRequest{
		Number: new(7),
		NodeID: new("test-node"),
		State:  new("open"),
		User:   &github.User{Login: new(publisherBot(mockApp))},
		Head: &github.PullRequestBranch{
			Ref:  new(releaseBranchPrefix + newTackCommit),
			SHA:  new(newTackCommit),
			Repo: &github.Repository{FullName: new(repoOwner + "/" + configsRepo)},
		},
		Base: &github.PullRequestBranch{Ref: new(mainBranch)},
	}
}

func TestTrustedCommitRejectsVerifiedSignatureFromAnotherCommitter(t *testing.T) {
	client := testGitHubClient(
		t,
		func(response http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/repos/agoodkind/configs/commits/"+newTackCommit {
				t.Fatalf("Unexpected request %s", request.URL.Path)
			}

			encodeResponse(
				t,
				response,
				map[string]any{
					"sha":       newTackCommit,
					"author":    map[string]string{"login": publisherBot(mockApp)},
					"committer": map[string]string{"login": "other-user"},
					"commit":    map[string]any{"verification": map[string]bool{"verified": true}},
					"parents":   []map[string]string{{"sha": oldTackCommit}},
				},
			)
		},
	)
	publisher := publisher{target: client, bot: publisherBot(mockApp)}
	if _, err := publisher.trustedCommit(t.Context(), newTackCommit); !errors.Is(
		err,
		errConfigsReleaseBranchRequiresVerified,
	) {
		t.Fatalf("Commit with another committer returned %v", err)
	}
}

func TestPublisherRejectsSameAppForReportingAndPublication(t *testing.T) {
	client := testGitHubClient(
		t,
		func(response http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet || request.URL.Path != "/apps/"+mockApp {
				t.Fatalf("Unexpected request %s %s", request.Method, request.URL.Path)
			}

			encodeResponse(t, response, map[string]any{"slug": mockApp, "id": reporterID})
		},
	)
	_, err := publishTack(t.Context(), client, client, "token", mockApp, reporterID, nil)
	if !errors.Is(err, errReporterApp) {
		t.Fatalf("Simultaneous publisher and reporter returned %v", err)
	}
}

func TestPublisherClosesCandidateSupersededDuringPRCreation(t *testing.T) {
	newerFinished := false
	closed := false
	oldBranch := releaseBranchPrefix + newTackCommit
	pull := map[string]any{
		"number":  27,
		"state":   "open",
		"node_id": "older-pr",
		"user":    map[string]string{"login": publisherBot(mockApp)},
		"head": map[string]any{
			"ref":  oldBranch,
			"repo": map[string]string{"full_name": "agoodkind/configs"},
		},
		"base": map[string]string{"ref": mainBranch},
	}
	client := testGitHubClient(
		t,
		func(response http.ResponseWriter, request *http.Request) {
			switch path := request.URL.Path; {
			case path == "/repos/agoodkind/configs/pulls" && request.Method == http.MethodPost:
				newerFinished = true
				encodeResponse(t, response, pull)
			case path == "/repos/agoodkind/configs/pulls" && request.Method == http.MethodGet:
				encodeResponse(t, response, []map[string]any{pull})
			case path == "/repos/agoodkind/configs/pulls/27" && request.Method == http.MethodPatch:
				var update github.PullRequest
				decodeRequest(t, request, &update)
				closed = update.GetState() == "closed"
				encodeResponse(t, response, pull)
			case path == "/repos/agoodkind/configs/git/ref/heads/main":
				encodeResponse(
					t,
					response,
					map[string]any{"object": map[string]string{"sha": oldTackCommit}},
				)
			case strings.HasPrefix(path, "/repos/agoodkind/configs/contents/"):
				content := releaseSelection(oldTackCommit)
				if strings.HasSuffix(path, artifactsPath) {
					content = fixtureArtifacts()
				}

				file := github.RepositoryContent{
					Type:     new("file"),
					Encoding: new("base64"),
					Content:  new(base64.StdEncoding.EncodeToString([]byte(content))),
				}
				encodeResponse(t, response, file)
			case path == "/repos/agoodkind/tack/compare/"+oldTackCommit+"..."+newTackCommit:
				encodeResponse(t, response, map[string]string{"status": "ahead"})
			case path == "/repos/agoodkind/tack/compare/"+newTackCommit+"...main":
				if !newerFinished {
					t.Fatal("Final eligibility ran before the PR creation response")
				}

				encodeResponse(
					t,
					response,
					map[string]any{
						"status":  "ahead",
						"commits": []map[string]string{{"sha": laterTackCommit}},
					},
				)
			case strings.HasPrefix(path, "/repos/agoodkind/tack/actions/workflows/"):
				encodeResponse(
					t,
					response,
					map[string]any{
						"workflow_runs": []map[string]any{successfulRun(laterTackCommit)},
					},
				)
			default:
				t.Fatalf("Stale candidate sent %s %s", request.Method, path)
			}
		},
	)
	publisher := publisher{
		source:        client,
		target:        client,
		bot:           publisherBot(mockApp),
		reporterAppID: reporterID,
	}
	manifest := release.Manifest{SourceSHA: newTackCommit, BuildRunID: 24, BuildAttempt: 1}
	pullRequest, err := publisher.retainRelease(
		t.Context(),
		oldBranch,
		manifest,
		nil,
		newTackCommit,
	)
	if err != nil || pullRequest != nil || !closed {
		t.Fatalf("Stale candidate returned %v, %v and closed=%v", pullRequest, err, closed)
	}
}

func TestDispatchRejectsUntrustedSenderAndOversizedEvent(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"action":         releaseDispatchAction,
		"repository":     map[string]string{"full_name": "agoodkind/configs"},
		"sender":         map[string]string{"login": "another-bot[bot]"},
		"client_payload": fixtureDispatch(releaseArtifact(t, newTackCommit)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := decodeDispatch(bytes.NewReader(body), publisherBot(mockApp)); !errors.Is(
		err,
		errDispatchIdentity,
	) {
		t.Fatalf("Dispatch from another sender returned %v", err)
	}

	oversized := strings.NewReader(strings.Repeat("x", maxDispatchEvent+1))
	if _, err := decodeDispatch(oversized, publisherBot(mockApp)); !errors.Is(
		err,
		errDispatchBounds,
	) {
		t.Fatalf("Oversized dispatch returned %v", err)
	}
}
