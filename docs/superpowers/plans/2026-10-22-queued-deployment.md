# Queued OpenTofu deployment implementation plan

> Use `subagent-driven-development` to implement each task and review its result. Track completion with checkbox syntax.

**Goal:** Publish PR plans and apply every merged release in order through validation and production environments.

**Architecture:** Reuse dflook actions for plans, applies, and PR reports. GitHub Actions runs the jobs; a Go executable selects releases and verifies deployment records. Consumers supply OpenTofu configuration, credentials, and checks.

**Tech stack:** GitHub Actions reusable workflows, Go, OpenTofu, dflook actions, Docker, and the GitHub REST API.

**Spec:** Read the [deployment design](../specs/2026-10-22-queued-deployment-design.md) before implementation.

## Global constraints

- Keep application details out of shared code.
- Use exactly two ordered stages in the first implementation: validation and production. Consumers choose their display labels, including QA.
- Run every PR revision independently from the release queue.
- Apply exact commits in first-parent order without combining or skipping releases.
- Generate and report fresh plans before applies; do not require another routine approval after merge.
- Complete deployment or verified recovery before processing another release.
- Keep provider credentials in protected consumer environment secrets and secrets out of outputs.
- Use YAML and existing actions for workflow composition. Use Go for additional logic. Do not add custom shell scripts.
- Pin action revisions and images. Record selected versions in the implementation commits.
- Use the existing shared Go build workflow for the new Go executable.
- Complete Tack runtime auditing before enabling Tack automatic deployment. Finish access lockdown afterward.

## Review focus

- A newer build finishes first: select the older pending commit and never deploy the newer commit first.
- A runner stops after apply but before reporting: inspect managed state before retrying or recovering.
- A recovery fails: stop that deployment set and preserve its incomplete record.
- A PR changes executable configuration: do not provide apply credentials or unrestricted production access to that execution.
- Suppress synthetic secret values from plan reports, comments, logs, summaries, and uploaded artifacts.

## Task 1: Define consumer inputs and build the Go action

Create the consumer schema and command boundary before writing workflow jobs. Keep the public schema versioned.

**Files:** Create [go.mod](../../../go.mod), [Makefile](../../../Makefile), [Dockerfile](../../../Dockerfile), [action.yml](../../../action.yml), and the Go packages below. These implementation files do not exist yet; the links specify their intended paths.

**Interfaces:**

- Define `config.DeploymentSet` in `internal/config/config.go` with `Version`, `ID`, `SourceBranch`, `InitialCommit`, `Configuration`, `Validation`, and `Production`.
- Define `config.Configuration` with `Repository`, `Revision`, `Root`, `TofuVersion`, and `ProviderLockDigest`.
- Define `config.Environment` with `Name`, `GitHubEnvironment`, `BackendFile`, `VariableFiles`, `HealthCheck`, and `RecoveryCheck`.
- Define `config.Check` with a pinned container `Image`, an `Arguments` string slice, and `TimeoutSeconds`. Reject shell programs and command-string inputs.
- Define `release.Manifest` in `internal/release/manifest.go` with `Version`, `Commit`, `ConfigurationRevision`, `Artifacts`, `Variables`, and `RequiredChecks`. Artifacts map stable names to immutable image digests. Variables contain typed nonsecret OpenTofu input values.
- Implement `config.Load(path string) (DeploymentSet, error)` and `release.Load(path string) (Manifest, error)` with strict unknown-field rejection.
- Create `internal/github/client.go` with `github.Client` and `NewClient(token string) *Client` for the GitHub REST API. The token comes from the job environment and must never appear in diagnostics.
- Implement `run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int` in `cmd/tofu-workflows/main.go`. Commands are `validate`, `select`, `record`, and `reconcile`; arguments identify input files and GitHub repository references. Credentials come from environment variables only.

- [ ] Add the shared Go build configuration and a Docker action that invokes the compiled executable directly.
- [ ] Validate required references, exact commits, image digests, disjoint environment state, and explicit timeouts.
- [ ] Add CLI tests using temporary real files. Missing inputs and unknown fields must fail before a GitHub or provider operation.
- [ ] Run `make build` and `go test ./cmd/tofu-workflows -run TestValidateConsumerInputs -count=1`. Assert a nonzero status and the missing field name for invalid files; assert successful validation for the complete synthetic consumer. Commit the input contract and executable boundary.

## Task 2: Publish plans for every PR commit

**Files:** Create [.github/workflows/plan.yml](../../../.github/workflows/plan.yml) and `internal/report/report.go`. Add the matching public-entry tests in `internal/acceptance/plan_test.go`.

**Interfaces:**

- Expose a reusable workflow with required inputs `configuration-repository`, `configuration-revision`, `configuration-root`, `deployment-set-file`, `release-manifest-artifact`, and `source-sha`.
- Require separate protected planning environments for the two stages. Their provider credentials permit the necessary resource reads and narrowly scoped backend locking, but not resource mutation.
- Trigger protected planning through `workflow_run` after the allowlisted PR build workflow finishes. Use the workflow definition from the protected deployment branch. Verify repository, workflow ID, PR head, artifact provenance, and typed release inputs. Select configuration and provider revisions from protected consumer configuration, not the PR artifact. Never execute PR code in this job.
- Define `report.Identity` with `Repository`, `PullRequest`, `Commit`, `Environment`, and `RunURL`.
- Implement `report.CanPublish(ctx context.Context, client *github.Client, identity Identity) (bool, error)` to compare the completed run's commit with the PR's current head. The `github.Client` package wraps real authenticated REST requests without accepting executable request templates.

- [ ] Check out the exact configuration revision and validate the release manifest before OpenTofu execution.
- [ ] Run `dflook/tofu-plan` for both environments with matching stable labels and `add_github_comment: false`.
- [ ] After planning, use `CanPublish` to prevent old runs from overwriting the current PR result. Publish a sanitized report using dflook's supported output where practical; add only the missing ordering protection and report fields in Go.
- [ ] Show result, change counts, commit, environment, expandable changes, and run links. Preserve earlier results as workflow runs.
- [ ] Keep plan files in ephemeral job storage. Do not upload binary plans or JSON containing sensitive values.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestPRPlanReportsNewestCommit -count=1` against a real test PR with two rapid commits. Assert that both commits start plans, the current PR comment identifies the newest commit, and neither plan changes the fixture's managed resources.
- [ ] Commit the workflow and its public-boundary verification.

## Task 3: Select and record releases in merge order

**Files:** Extend `internal/github/client.go`. Create `internal/queue/select.go`, `internal/queue/reconcile.go`, `internal/deployment/record.go`, and `internal/acceptance/queue_test.go`.

**Interfaces:**

- Define `queue.Request` with `Repository`, `DeploymentSetID`, `Branch`, `InitialCommit`, and `RunURL`.
- Define `queue.Decision` with `State`, `Commit`, `PullRequest`, `ManifestArtifactID`, and `Reason`. `State` is one of `ready`, `waiting`, `empty`, or `stopped`.
- Implement `queue.Select(ctx context.Context, client *github.Client, request Request) (Decision, error)` using first-parent commit order and GitHub deployment records.
- Define `deployment.Attempt` with `SchemaVersion`, `Repository`, `DeploymentSetID`, `Commit`, `Stage`, `ConfigurationRevision`, `ArtifactDigests`, `PreviousCommit`, and `RunURL`.
- Use `Stage` values `validation`, `production`, `validation-recovery`, and `production-recovery`. Save these fields in the GitHub deployment payload before starting the corresponding operation.
- Implement `deployment.Begin(ctx context.Context, client *github.Client, attempt Attempt) (int64, error)` and `deployment.Finish(ctx context.Context, client *github.Client, id int64, outcome Outcome) error`. Outcomes use GitHub deployment statuses and preserve failed releases separately from successful recovery attempts.
- Define `deployment.Outcome` with `State` and `Description` strings and `FinishedAt time.Time`. Accept only GitHub deployment states; report recovery through its own attempt rather than changing a failed release to success.
- Implement `queue.Reconcile(ctx context.Context, client *github.Client, request Request) (Decision, error)`. It discovers unfinished commits from Git history instead of treating trigger delivery as the queue's source of truth.

- [ ] Select the oldest unfinished mainline commit after the verified baseline. Resolve all inputs from that selected commit.
- [ ] Reuse an existing attempt after an uncertain API response. Do not create a second apply because a status request timed out.
- [ ] Return `waiting` when the oldest release lacks required artifacts or successful checks; do not select a later release.
- [ ] Return `stopped` when an incomplete apply or unsuccessful recovery needs investigation.
- [ ] Configure the consumer entry workflow to use one workflow-level concurrency group with `queue: max` and active cancellation disabled. The group must cover selection and both deployment stages.
- [ ] Trigger reconciliation on release-build completion, deployment completion, manual retry, and a 15-minute schedule. Treat the schedule interval as an agent-selected recovery setting.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestMergeOrderWithReversedBuildCompletion -count=1`. Assert one verified deployment per commit in merge order for three real merged test PRs.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestReconcileMissingAndDuplicateTriggers -count=1`. Assert recovery of omitted triggers and unchanged apply counts after duplicates. Commit the queue component.

## Task 4: Apply, promote, and recover

**Files:** Create [.github/workflows/deploy.yml](../../../.github/workflows/deploy.yml), `internal/check/run.go`, `internal/recovery/recovery.go`, and `internal/acceptance/deploy_test.go`.

**Interfaces:**

- Reuse Task 1 configuration and Task 3 decisions. Do not add application-specific branches to the shared workflow.
- Define `check.Result` with `Passed`, `ObservedArtifacts`, `Summary`, and `FinishedAt`.
- Implement `check.Run(ctx context.Context, definition config.Check, manifest release.Manifest) (Result, error)` through the Docker Go SDK. Pass arguments as an array and enforce the configured deadline.
- Define `recovery.Request` with `FailedAttempt`, `PreviousManifest`, and `ObservedState`. Define `recovery.Decision` with `Allowed`, `Manifest`, and `Reason`.
- Use `deployment.Attempt`, `release.Manifest`, and `check.Result` for those request fields. Use `bool`, `release.Manifest`, and `string` for the decision fields. Add `CompatibleWithPrevious bool` to `check.Result`; require the consumer check to verify this against its real managed state.
- Implement `recovery.Assess(request Request) (Decision, error)`. Permit only a consumer-supported restoration of application release values with confirmed database compatibility. Reject database reversal or destructive restoration.

- [ ] Select one release under the complete workflow concurrency exclusion.
- [ ] For validation, create a fresh plan for that exact release, report it, and apply its saved plan with dflook. Configure `auto_approve` for the accepted merge authorization rather than comparing against the older PR preview.
- [ ] Verify installed artifacts and runtime behavior, then record the result.
- [ ] Execute production only after validation success. Use production's own backend, plan, credentials, and checks.
- [ ] On a mutation or health failure, inspect actual state, evaluate recovery, plan the previous compatible application values, apply that recovery plan, and check the restored service.
- [ ] Keep original failure and recovery results distinct. Stop the deployment set when recovery fails or actual state cannot be established.
- [ ] On a retry after interruption, require actual state verification before repeating an operation.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run 'TestPromotion|TestFailedQA|TestCompatibleRecovery|TestFailedRecovery|TestInterruptedApply' -count=1`. Assert the installed version, production mutation count, retained test data, recovery result, and queue state for the relevant scenario.
- [ ] Commit the deployment workflow and recovery behavior.

## Task 5: Prove isolation and publish the reusable workflows

**Files:** Create [.github/workflows/acceptance.yml](../../../.github/workflows/acceptance.yml), fixture configuration under `internal/acceptance/testdata`, and usage documentation in [README.md](../../../README.md).

**Interfaces:**

- Supply a disposable OpenTofu root, two independent backend states, and a small Go HTTP application with good and deliberately failing release variants.
- Use a dedicated GitHub test consumer with synthetic credentials. Select its repository explicitly through `TOFU_WORKFLOWS_TEST_REPOSITORY`; never default to a production consumer.
- Make the fixture's health check assert the installed version and an observable request result. After recovery, assert both the previous version and preserved test data.

- [ ] Run the complete acceptance matrix from the design using public workflow events and real GitHub APIs. Do not replace GitHub, OpenTofu, Docker, or storage operations with mocks.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestUntrustedPRCannotUseProtectedCredentials -count=1`. Include a synthetic disclosure attempt and assert that protected jobs do not execute that source with privileged credentials.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestSecretValuesAbsentFromPublishedResults -count=1`. Assert that public results and downloadable artifacts do not disclose the fixture's synthetic secret values.
- [ ] Document the consumer contract, required permissions, two-stage configuration, baseline verification, interruption recovery, and immutable references.
- [ ] Publish a reviewed immutable workflow revision only after the real acceptance runs pass. Do not enable a production consumer during library acceptance.
- [ ] Commit the acceptance evidence references and usage documentation.

## Task 6: Integrate the first consumer separately

Tack is the first consumer. Its [declarative deployment specification](https://github.com/agoodkind/tack/blob/2b1d90ea65519e978ce8e425f3228c9c0542593d/docs/superpowers/specs/2026-10-10-tack-provider.md) requires existing general providers for supported resources and Tack resources only for application-specific behavior.

The inspected merged implementation registers only the database-migration resource. It does not yet supply complete application deployment through OpenTofu. Complete that existing provider work and its integration plan before enabling automatic Tack applies. This plan implements the generic workflow library; it does not silently expand into a replacement provider or full infrastructure migration.

- [ ] Create a separate Tack integration plan that maps the existing deployment fields to their owning providers and supplies the exact root, backend, credentials, release manifest, and checks required by Task 1.
- [ ] Replace any proposed SSH-only deployment wrapper with the actual provider authentication and transport. Verify its permissions before enabling live PR planning.
- [ ] Add the small Tack caller workflow at the pinned shared revision after the runtime-audit implementation completes.
- [ ] Verify one QA deployment, production promotion, and compatible recovery with exact installed-image evidence.
- [ ] Keep Configs merge-triggered applies deferred. A pinned configuration revision may be an input without making Configs merges deployment triggers.
