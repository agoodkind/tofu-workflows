package tack

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
)

const (
	oldTackCommit     = "1111111111111111111111111111111111111111"
	newTackCommit     = "2222222222222222222222222222222222222222"
	laterTackCommit   = "3333333333333333333333333333333333333333"
	mockApp           = "release-app"
	reporterID        = 783
	fixtureArtifactID = 432
)

func TestPublishTackCreatesAndReconcilesOneReadyPR(t *testing.T) {
	release := newReleaseFixture(t)
	artifact := releaseArtifact(t, newTackCommit)

	urls := release.publish(t, artifact)
	if len(urls) != 1 || urls[0] != "https://github.com/agoodkind/configs/pull/27" {
		t.Fatalf("PublishTack returned %v", urls)
	}

	if release.opens != 1 || release.signedCommits != 1 || release.autoMerges != 0 {
		t.Fatalf(
			"First publication opened %d PRs, signed %d commits and enabled %d merges",
			release.opens,
			release.signedCommits,
			release.autoMerges,
		)
	}

	if release.files[qaPath] != releaseSelection(newTackCommit) ||
		release.files[prodPath] != releaseSelection(newTackCommit) {
		t.Fatalf("Publication did not pin both Configs environments: %+v", release.files)
	}

	if !strings.Contains(release.files[artifactsPath], "config-owner@example.com") ||
		!strings.Contains(release.files[artifactsPath], strings.Repeat("b", 64)) ||
		!strings.Contains(release.files[artifactsPath], strings.Repeat("d", 64)) {
		t.Fatalf(
			"Publication changed the accountable owner or lost release references: %s",
			release.files[artifactsPath],
		)
	}

	release.allowAuto = true
	release.boundReporter = reporterID + 1
	release.publish(t, artifact)
	if release.opens != 1 || release.signedCommits != 1 || release.autoMerges != 0 {
		t.Fatalf("Redelivery wrote another PR or signed commit or accepted the wrong reporter")
	}

	previousHead := release.prHead
	release.listAutoMerge = true
	release.advanceBase(t)
	release.boundReporter = reporterID
	release.publish(t, nil)
	if release.opens != 1 || release.signedCommits != 2 || release.autoMerges != 1 {
		t.Fatalf(
			"Reconciliation opened %d PRs, signed %d commits and enabled %d merges",
			release.opens,
			release.signedCommits,
			release.autoMerges,
		)
	}

	if release.prHead == previousHead {
		t.Fatal("Reconciliation did not refresh the Configs commit")
	}

	parent := release.git(t, "rev-parse", "refs/heads/"+releaseBranchPrefix+newTackCommit+"^")
	if parent != release.base {
		t.Fatalf("Reconciled PR parent %s does not match Configs main %s", parent, release.base)
	}
}

func TestPublishTackRejectsSupersededBuilds(t *testing.T) {
	release := newReleaseFixture(t)
	release.newerSucceeded = true
	if urls := release.publish(t, releaseArtifact(t, newTackCommit)); len(urls) != 0 {
		t.Fatalf("Superseded Tack release returned %v", urls)
	}

	if release.opens != 0 || release.signedCommits != 0 {
		t.Fatalf(
			"Superseded release opened %d PRs and signed %d commits",
			release.opens,
			release.signedCommits,
		)
	}
}

func TestPublishTackKeepsReleaseWhenNewerBuildFailed(t *testing.T) {
	release := newReleaseFixture(t)
	release.newerFailed = true
	if urls := release.publish(t, releaseArtifact(t, newTackCommit)); len(urls) != 1 {
		t.Fatalf("Eligible release returned %v after a newer failed build", urls)
	}
}

func TestPublishTackVerifiesTheBuildAttemptAndSource(t *testing.T) {
	release := newReleaseFixture(t)
	_, err := publishTack(
		t.Context(),
		release.client,
		release.client,
		"synthetic-token",
		mockApp,
		reporterID,
		fixtureDispatch(releaseArtifact(t, laterTackCommit)),
	)
	if err == nil ||
		!strings.Contains(err.Error(), "does not match the Tack build artifact") {
		t.Fatalf("Unexpected source revision produced error %v", err)
	}

	if release.opens != 0 || release.signedCommits != 0 {
		t.Fatal("Unverified release changed Configs")
	}
}

func TestPushReleaseRefRequiresExpectedHead(t *testing.T) {
	release := newReleaseFixture(t)
	candidate := "prepared"
	release.git(t, "update-ref", "refs/heads/"+candidate, release.base)
	release.advanceBase(t)
	release.git(t, "update-ref", "refs/heads/release", release.base)

	err := pushReleaseRef(
		context.Background(),
		t.TempDir(),
		release.directory,
		"synthetic-token",
		candidate,
		release.git(t, "rev-parse", "refs/heads/"+candidate),
		"release",
		laterTackCommit,
	)
	if err == nil || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("Branch update with a stale lease returned %v", err)
	}

	actual := release.git(t, "rev-parse", "refs/heads/release")
	if actual != release.base {
		t.Fatalf("Stale branch update moved release to %s", actual)
	}
}

func releaseArtifact(t *testing.T, source string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create("release.json")
	if err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"version":       1,
		"source_sha":    source,
		"build_run_id":  24,
		"build_attempt": 1,
		"artifacts": map[string]string{
			"server":   serverRepository + "@sha256:" + strings.Repeat("a", 64),
			"consumer": consumerRepository + "@sha256:" + strings.Repeat("b", 64),
		},
		"variables": map[string]string{
			fdbOverlay:      strings.Repeat("c", 64),
			yugabyteOverlay: strings.Repeat("d", 64),
		},
	}
	if err := json.NewEncoder(entry).Encode(manifest); err != nil {
		t.Fatal(err)
	}

	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

type releaseFixture struct {
	client         *github.Client
	directory      string
	base           string
	prHead         string
	commitMessages map[string]string
	files          map[string]string
	signedCommits  int
	opens          int
	autoMerges     int
	listAutoMerge  bool
	boundReporter  int
	allowAuto      bool
	newerSucceeded bool
	newerFailed    bool
	archive        []byte
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()

	fixture := &releaseFixture{
		directory:      t.TempDir(),
		commitMessages: make(map[string]string),
		archive:        releaseArtifact(t, newTackCommit),
	}
	fixture.git(t, "init", "--bare", "--quiet")
	fixture.git(t, "config", "user.name", "Test Author")
	fixture.git(t, "config", "user.email", "fixture@example.com")
	fixture.advanceBase(t)
	configFile := filepath.Join(t.TempDir(), "gitconfig")
	config := fmt.Sprintf(
		"[url %q]\n  insteadOf = %s\n",
		"file://"+fixture.directory,
		configsRemote,
	)
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_CONFIG_GLOBAL", configFile)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	fixture.client = testGitHubClient(
		t,
		func(writer http.ResponseWriter, request *http.Request) {
			fixture.handle(t, writer, request)
		},
	)

	return fixture
}

func fixtureDispatch(archive []byte) *releaseDispatch {
	if archive == nil {
		return nil
	}

	return &releaseDispatch{Archive: archive, ArtifactID: fixtureArtifactID, RunID: 24, Attempt: 1}
}

type githubResponseFunc func(*http.Request) *http.Response

func (respond githubResponseFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return respond(request), nil
}

func testGitHubClient(t *testing.T, handle http.HandlerFunc) *github.Client {
	t.Helper()
	client, err := github.NewClient(
		github.WithHTTPClient(&http.Client{
			Transport: githubResponseFunc(func(request *http.Request) *http.Response {
				recorder := httptest.NewRecorder()
				handle(recorder, request)
				response := recorder.Result()
				response.Request = request

				return response
			}),
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func (fixture *releaseFixture) publish(t *testing.T, archive []byte) []string {
	t.Helper()
	urls, err := publishTack(
		t.Context(),
		fixture.client,
		fixture.client,
		"synthetic-token",
		mockApp,
		reporterID,
		fixtureDispatch(archive),
	)
	if err != nil {
		t.Fatal(err)
	}

	return urls
}

func (fixture *releaseFixture) git(t *testing.T, args ...string) string {
	t.Helper()

	result, err := fixture.gitResult(args...)
	if err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, result)
	}

	return result
}

func (fixture *releaseFixture) gitResult(args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = fixture.directory
	result, err := command.CombinedOutput()

	return strings.TrimSpace(string(result)), err
}

func (fixture *releaseFixture) advanceBase(t *testing.T) {
	t.Helper()

	tree := fixture.git(t, "hash-object", "-w", "-t", "tree", "/dev/null")
	args := []string{"commit-tree", tree}
	if fixture.base != "" {
		args = append(args, "-p", fixture.base)
	}

	fixture.base = fixture.git(t, append(args, "-m", "Configs main")...)
	fixture.git(t, "update-ref", "refs/heads/main", fixture.base)
}

func (fixture *releaseFixture) handle(t *testing.T, writer http.ResponseWriter, r *http.Request) {
	t.Helper()

	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	switch {
	case path == "/apps/"+mockApp:
		encodeResponse(t, writer, map[string]any{"slug": mockApp, "id": reporterID + 20})
	case path == "/repos/agoodkind/tack/actions/artifacts/432":
		encodeResponse(
			t,
			writer,
			map[string]any{
				"id":      fixtureArtifactID,
				"name":    "tack-release-1",
				"expired": false,
				"digest":  fmt.Sprintf("sha256:%x", sha256.Sum256(fixture.archive)),
				"workflow_run": map[string]any{
					"id":                 24,
					"head_sha":           newTackCommit,
					"repository_id":      tackRepositoryID,
					"head_repository_id": tackRepositoryID,
				},
			},
		)
	case path == "/repos/agoodkind/tack/actions/runs/24/attempts/1":
		encodeResponse(t, writer, successfulRun(newTackCommit))
	case strings.HasPrefix(path, "/repos/agoodkind/tack/compare/"):
		compared := strings.TrimPrefix(path, "/repos/agoodkind/tack/compare/")
		if compared == newTackCommit+"...main" {
			fixture.newerComparison(t, writer)

			return
		}

		encodeResponse(t, writer, map[string]string{"status": "ahead"})
	case strings.HasPrefix(path, "/repos/agoodkind/tack/actions/workflows/"):
		runs := []map[string]any{}
		if fixture.newerSucceeded {
			runs = append(runs, successfulRun(laterTackCommit))
		}

		encodeResponse(t, writer, map[string]any{"workflow_runs": runs})
	case strings.HasPrefix(path, "/repos/agoodkind/configs/contents/"):
		file := strings.TrimPrefix(path, "/repos/agoodkind/configs/contents/")
		content := releaseSelection(oldTackCommit)
		if file == artifactsPath {
			content = fixtureArtifacts()
		}

		encodeResponse(
			t,
			writer,
			map[string]string{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			},
		)
	case strings.HasPrefix(
		path,
		"/repos/agoodkind/configs/git/ref/heads/",
	) && r.Method == http.MethodGet:
		branch := strings.TrimPrefix(path, "/repos/agoodkind/configs/git/ref/")
		sha, err := fixture.gitResult("rev-parse", "refs/"+branch)
		if err != nil {
			http.Error(writer, "missing ref", http.StatusNotFound)

			return
		}

		encodeResponse(t, writer, map[string]any{"object": map[string]string{"sha": sha}})
	case path == "/repos/agoodkind/configs/git/refs" && r.Method == http.MethodPost:
		var create github.CreateRef
		decodeRequest(t, r, &create)
		fixture.git(t, "update-ref", create.Ref, create.SHA)
		encodeResponse(t, writer, map[string]string{"ref": create.Ref})
	case strings.HasPrefix(
		path,
		"/repos/agoodkind/configs/git/refs/heads/",
	) && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/repos/agoodkind/configs/git/")
		fixture.git(t, "update-ref", "-d", ref)
		writer.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/repos/agoodkind/configs/commits/"):
		fixture.commitResponse(
			t,
			writer,
			strings.TrimPrefix(path, "/repos/agoodkind/configs/commits/"),
		)
	case path == "/repos/agoodkind/configs/pulls" || path == "/repos/agoodkind/configs/pulls/27":
		fixture.pullResponse(t, writer, r)
	case path == "/repos/agoodkind/configs":
		encodeResponse(t, writer, map[string]bool{"allow_auto_merge": fixture.allowAuto})
	case path == "/repos/agoodkind/configs/rules/branches/main":
		encodeResponse(
			t,
			writer,
			[]map[string]any{
				{
					"type": "required_status_checks",
					"parameters": map[string]any{
						"required_status_checks": []map[string]any{
							{
								"context":        "Terrakube plan",
								"integration_id": fixture.boundReporter,
							},
						},
					},
				},
			},
		)
	case path == "/graphql":
		fixture.graphQLResponse(t, writer, r)
	default:
		t.Errorf("Unexpected GitHub request %s %s", r.Method, path)
		http.Error(writer, "unexpected request", http.StatusNotFound)
	}
}

func successfulRun(source string) map[string]any {
	return map[string]any{
		"id":              24,
		"run_attempt":     1,
		"head_sha":        source,
		"head_branch":     "main",
		"workflow_id":     tackBuildWorkflowID,
		"path":            tackBuildWorkflow,
		"status":          "completed",
		"conclusion":      "success",
		"event":           "push",
		"head_repository": map[string]string{"full_name": repoOwner + "/" + tackRepo},
	}
}

func (fixture *releaseFixture) newerComparison(t *testing.T, writer http.ResponseWriter) {
	t.Helper()

	if fixture.newerSucceeded || fixture.newerFailed {
		encodeResponse(
			t,
			writer,
			map[string]any{
				"status":  "ahead",
				"commits": []map[string]string{{"sha": laterTackCommit}},
			},
		)

		return
	}

	encodeResponse(t, writer, map[string]string{"status": "identical"})
}

func (fixture *releaseFixture) commitResponse(
	t *testing.T,
	writer http.ResponseWriter,
	sha string,
) {
	t.Helper()

	parent := fixture.git(t, "rev-parse", sha+"^")
	encodeResponse(
		t,
		writer,
		map[string]any{
			"sha":       sha,
			"author":    map[string]string{"login": mockApp + "[bot]"},
			"committer": map[string]string{"login": githubWebCommitter},
			"commit": map[string]any{
				"message":      fixture.commitMessages[sha],
				"verification": map[string]bool{"verified": true},
			},
			"parents": []map[string]string{{"sha": parent}},
		},
	)
}

func (fixture *releaseFixture) pullResponse(
	t *testing.T,
	writer http.ResponseWriter,
	r *http.Request,
) {
	t.Helper()

	ref := releaseBranchPrefix + newTackCommit
	if r.Method == http.MethodPost {
		var create github.CreatePullRequest
		decodeRequest(t, r, &create)
		if create.GetDraft() || create.Head != ref || create.Base != mainBranch {
			t.Errorf("Invalid release PR request: %+v", create)
		}

		fixture.opens++
	}

	if fixture.opens == 0 {
		encodeResponse(t, writer, []map[string]any{})

		return
	}

	fixture.prHead = fixture.git(t, "rev-parse", "refs/heads/"+ref)
	pull := map[string]any{
		"number":   27,
		"node_id":  "test-node",
		"state":    "open",
		"draft":    false,
		"html_url": "https://github.com/agoodkind/configs/pull/27",
		"user":     map[string]string{"login": mockApp + "[bot]"},
		"head": map[string]any{
			"ref":  ref,
			"sha":  fixture.prHead,
			"repo": map[string]string{"full_name": "agoodkind/configs"},
		},
		"base": map[string]string{"ref": "main"},
	}
	if r.Method == http.MethodPost || strings.HasSuffix(r.URL.Path, "/pulls/27") {
		encodeResponse(t, writer, pull)

		return
	}

	if fixture.listAutoMerge {
		pull["auto_merge"] = map[string]string{"merge_method": "merge"}
	}

	encodeResponse(t, writer, []map[string]any{pull})
}

func (fixture *releaseFixture) graphQLResponse(
	t *testing.T,
	writer http.ResponseWriter,
	r *http.Request,
) {
	t.Helper()

	var request struct {
		Query     string
		Variables struct{ Input json.RawMessage }
	}
	decodeRequest(t, r, &request)
	if strings.Contains(request.Query, "wasSignedByGitHub") {
		encodeResponse(
			t,
			writer,
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"object": map[string]any{
							"signature": map[string]any{"isValid": true, "wasSignedByGitHub": true},
						},
					},
				},
			},
		)

		return
	}

	if strings.Contains(request.Query, "enablePullRequestAutoMerge") {
		var input map[string]string
		if err := json.Unmarshal(request.Variables.Input, &input); err != nil {
			t.Fatal(err)
		}

		if input["expectedHeadOid"] != fixture.prHead {
			t.Fatalf(
				"Auto-merge used head %q, expected %q",
				input["expectedHeadOid"],
				fixture.prHead,
			)
		}

		fixture.autoMerges++
		encodeResponse(t, writer, map[string]any{"data": map[string]any{}})

		return
	}

	var input commitInput
	if err := json.Unmarshal(request.Variables.Input, &input); err != nil {
		t.Error(err)
	}

	if !strings.Contains(request.Query, "createCommitOnBranch") ||
		input.ExpectedHeadOID != fixture.base {
		t.Errorf("Invalid signed commit request: %+v", input)
	}

	fixture.files = make(map[string]string)

	for _, file := range input.FileChanges["additions"] {
		decoded, err := base64.StdEncoding.DecodeString(file.Contents)
		if err != nil {
			t.Error(err)
		}

		fixture.files[file.Path] = string(decoded)
	}

	branch := input.Branch["branchName"]
	tree := fixture.git(t, "hash-object", "-w", "-t", "tree", "/dev/null")
	message := input.Message["headline"] + "\n\n" + input.Message["body"]
	sha := fixture.git(t, "commit-tree", tree, "-p", fixture.base, "-m", message)
	fixture.git(t, "update-ref", "refs/heads/"+branch, sha)
	fixture.signedCommits++
	fixture.commitMessages[sha] = message
	encodeResponse(
		t,
		writer,
		map[string]any{
			"data": map[string]any{
				"createCommitOnBranch": map[string]any{
					"commit": map[string]any{
						"oid": sha,
						"signature": map[string]bool{
							"isValid": true,
						},
					},
				},
			},
		},
	)
}

func releaseSelection(commit string) string {
	return "locals { release_commit = " + strconv.Quote(commit) + " }\n"
}

func fixtureArtifacts() string {
	return fmt.Sprintf(
		`output "tack_overlays" {
  value = {
    %q = %q
    %q = %q
  }
}

output "tack_releases" {
  value = {
    %q = {
      commit          = %q
      server_digest   = "sha256:%s"
      consumer_digest = "sha256:%s"
      accountable     = "config-owner@example.com"
    }
  }
}
`,
		fdbOverlay,
		strings.Repeat("e", 64),
		yugabyteOverlay,
		strings.Repeat("e", 64),
		oldTackCommit,
		oldTackCommit,
		strings.Repeat("e", 64),
		strings.Repeat("e", 64),
	)
}

func encodeResponse(t *testing.T, writer http.ResponseWriter, response any) {
	t.Helper()

	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		t.Error(err)
	}
}

func decodeRequest(t *testing.T, request *http.Request, output any) {
	t.Helper()

	if err := json.NewDecoder(request.Body).Decode(output); err != nil {
		t.Error(err)
	}
}
