# Queued OpenTofu deployment implementation plan

> Use `subagent-driven-development` to implement each task and review its result. Track completion with checkbox syntax.

**Goal:** Publish PR plans and apply every merged release in order through validation and production environments.

**Architecture:** Reuse configsctl for OpenTofu execution, redaction, cancellation, and host locks. GitHub Actions runs the jobs; a Go executable selects releases, reports plans, and verifies deployment records. Consumers supply OpenTofu configuration, credentials, and checks.

**Tech stack:** GitHub Actions reusable workflows, Go, OpenTofu, configsctl, Docker, and the GitHub REST API.

**Spec:** Read the [deployment design](../specs/2026-10-22-queued-deployment-design.md) and the [implementation contracts](2026-10-22-deployment-contracts.md). The contracts define exact types, pinned dependencies, permissions, queue transitions, and recovery behavior for every task.

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
- Identify each environment by configuration root, backend state, and existing CLI workspace. Do not apply unrelated infrastructure to release one application.

## Component responsibilities

| Component | Responsibility | Task |
| --- | --- | --- |
| configsctl public `tofurun` and `hostlock` packages | Reuse command execution and locking with explicit CI inputs. | 0 |
| `cmd/tofu-workflows` and `action.yml` | Invoke the compiled Go action. | 1 |
| `internal/config`, `internal/release`, `internal/credentials` | Validate configuration and release metadata; install declared credential variables. | 1 |
| `internal/githubapi` | Configure the maintained GitHub SDK and enforce safe errors and authorization. | 1 |
| `internal/access`, `internal/preview`, `internal/tofu` | Connect fixed private endpoints and adapt validated inputs to configsctl. | 2 |
| `internal/report` and the plan workflow | Publish results for the correct PR revision. | 2 |
| `internal/queue`, `internal/deployment` | Select releases and record intent and final outcomes. | 3 |
| `internal/check`, `internal/recovery` and the deploy workflow | Inspect actual state, apply releases, and recover compatible application values. | 4 |
| `internal/execution`, `environment/Dockerfile` and `environment/action.yml` | Run one environment sequence with the existing configsctl lock library. | 4 |
| `internal/acceptance` | Exercise real workflows and managed resources. | 5 |

Tasks 0 and 1 may proceed independently. Task 3 consumes Task 1 and may proceed alongside Task 0. Task 2 requires Tasks 0 and 1; Task 4 requires Tasks 0, 2, and 3. Task 5 verifies their integration. Task 6 establishes consumer readiness separately.

## Review focus

- A newer build finishes first: select the older pending commit and never deploy the newer commit first.
- A runner stops after apply but before reporting: inspect managed state before retrying or recovering.
- A recovery fails: stop that deployment set and preserve its incomplete record.
- A PR changes executable configuration: do not provide apply credentials or unrestricted production access to that execution.
- Suppress synthetic secret values from plan reports, comments, logs, summaries, and uploaded artifacts.

## Task 0: Expose the existing configsctl runner for CI

**Files:** In configsctl, extract execution into `tofurun/{run,environment,output}.go`, expose `hostlock`, and add `cmd/configsctl/ci.go` and focused CI acceptance tests. Modify the existing main dispatch, execution, redaction, and lock callers to consume that same implementation. Preserve existing local behavior.

**Interfaces:** Implement the exact `tofurun.Request`, `Result`, `OutputMode`, `Run`, opaque `hostlock.Scope`, restricted transport, strict renewal, and checked release contracts. Consume explicit environment assignments and connection settings rather than the Configs vault or inventory. The shared helper links the public packages and keeps one Set for the complete environment sequence.

- [ ] Dispatch the read-only `ci tofu` CLI before vault setup. Use the same public runner from the existing local command and the workflow. Preserve detailed exit status two and the exact saved-plan file. Do not add an executable shim, lock bypass, second supervisor, or new shell script.
- [ ] Add `TestCIRunsWithoutVaultOrInventory` using real OpenTofu, temporary HOME/configuration, and an OS file-access trace. Assert successful init/plan and absence of reads of the unrelated synthetic vault and password files. Add `TestCIEnvironmentIsExplicit` using a real disposable provider HTTP service; assert the provider receives declared synthetic credentials and cannot observe unrelated parent values.
- [ ] Add `TestCISavedPlanAndStaleState` through the public runner with real temporary state and acquired locks. Assert exact-plan apply and stale-plan rejection without another plan. Add `TestCIRequiresOwnedScope` for nil, foreign-process, released, expired, and incomplete-target scopes and a forged `CONFIGS_LOCK_STATE`; assert unchanged managed resources.
- [ ] Add `TestCIProgressAndPrivateJSON` with actual provider diagnostics containing synthetic secrets and split writes. Assert redacted live progress/logs, valid exact mode-0600 JSON, correct detailed statuses, and secret-free public reports. Add `TestCICancellationStopsProviderGroup` with SIGTERM, real child processes, independent cleanup, and checked release/abandon outcomes.
- [ ] Preserve the real SSH/filesystem interoperability cases from the contracts. Run `make check`, `make test`, and the owning repository's explicit real-dependency integration target. Missing required tools fail the CI acceptance rather than silently skipping it. Publish and pin the completed module only after those results pass. Commit the package extraction and CI behavior as separate coherent changes.

## Task 1: Define consumer inputs and build the Go action

Create the consumer schema and command boundary before writing workflow jobs. Keep the public schema versioned.

**Files:** Create [go.mod](../../../go.mod), [Makefile](../../../Makefile), [Dockerfile](../../../Dockerfile), [action.yml](../../../action.yml), and the Go packages below. These implementation files do not exist yet; the links specify their intended paths.

**Interfaces:**

- Define the exact configuration types from the contracts in `internal/config/config.go` and `release.Manifest` in `internal/release/manifest.go`. Treat the contracts as their single definition.
- Implement `config.Load(path string) (DeploymentSet, error)` and `release.Load(path string) (Manifest, error)` with strict unknown-field rejection.
- Implement `config.ValidateStateOwnership(set DeploymentSet, baseDir string) error` by resolving the configured backend files and CLI workspaces. Reject duplicate physical state identities even when their environment labels differ.
- Implement `release.ReadArtifact(reader io.Reader, expectedRunID int64, expectedAttempt int) (Manifest, error)` with the archive limits and provenance checks from the contracts.
- Implement `credentials.Install(raw string, allowed []string, files map[string]string, environmentFile string) error` in `internal/credentials/install.go`. Follow the contracts' file bindings, permissions, multiline encoding, and cleanup rules.
- Create `internal/githubapi/client.go` with `NewClient(token string) (*github.Client, error)` using `github.com/google/go-github/v93/github`. Reject an empty token. Use the v93 option-based `github.NewClient`, including `github.WithAuthToken` and `github.WithHTTPClient` with a bounded timeout; it returns the client and an error. Use SDK API models, services, iterators, and typed errors throughout the following tasks. Report safe status codes and endpoint paths instead of raw authenticated responses. Do not implement another REST client. In the signatures below, `github.Client` refers to this SDK type.
- Implement `run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int` in `cmd/tofu-workflows/main.go`. Commands are `validate`, `credentials`, `preview`, `environment`, `cleanup`, `report`, `select`, `record`, `inspect`, `recover`, and `reconcile`; arguments identify validated files beneath the job workspace and GitHub references. Credentials come from environment variables only.

- [ ] Add the shared Go build configuration and a Docker action that invokes the compiled executable directly. Use the pinned official OpenTofu image and verified cloudflared from the contracts; link the completed configsctl packages.
- [ ] Validate required references, exact commits, image digests, disjoint environment state, and explicit timeouts.
- [ ] Install the backend and provider bundles separately against their disjoint allowlists. Reuse the same JSON validation, masking, environment-file encoding, and selected-file handling for both.
- [ ] Add CLI tests using temporary real files. Missing inputs and unknown fields must fail before a GitHub or provider operation.
- [ ] Add `TestRejectUnsafeManifestArchive` through the CLI with traversal, duplicate entries, a forged run ID, an oversized entry, and an undeclared variable. Assert failure and absence of extracted files outside the temporary job directory.
- [ ] Run `make build` and `go test ./cmd/tofu-workflows -run TestValidateConsumerInputs -count=1`. Assert a nonzero status and the missing field name for invalid files; assert successful validation for the complete synthetic consumer. Commit the input contract and executable boundary.

## Task 2: Publish plans for every PR commit

**Files:** Create [.github/workflows/plan.yml](../../../.github/workflows/plan.yml), `internal/access/access.go`, `internal/preview/run.go`, `internal/tofu/run.go`, and `internal/report/report.go`. Add the matching public-entry tests in `internal/acceptance/plan_test.go`.

**Interfaces:**

- Expose a reusable workflow with required inputs `deployment-set-file`, `producer-run-id`, and `producer-run-attempt`. Read protected consumer configuration from the deployment branch. A PR artifact cannot select a configuration root or provider revision.
- Require separate protected planning environments for the two stages. Their provider credentials permit the necessary resource reads and narrowly scoped backend locking, but not resource mutation.
- Trigger protected planning through `workflow_run` after the allowlisted PR build workflow finishes. Use the workflow definition from the protected deployment branch. Verify repository, workflow ID, PR head, artifact provenance, and typed release inputs. Select configuration and provider revisions from protected consumer configuration, not the PR artifact. Never execute PR code in this job.
- Define `report.Identity` with `Repository`, `PullRequest`, `Commit`, `Environment`, and `RunURL`.
- Define the identity fields as strings except `PullRequest int`. Implement `report.Publish(ctx context.Context, client *github.Client, identity Identity, plan Plan) error` under the PR/environment concurrency group. Verify the current head and comment author before writing.
- Define `report.Plan` with `Succeeded bool`, `Changed bool`, `Counts map[string]int`, `RedactedDiff string`, and `ErrorCode string`. Implement `report.ReadPlan(path string, changed bool) (Plan, error)` with sensitivity masking and the specified output limits.
- Define `tofu.SavedPlan{Path, JSONPath string, Changed bool, Counts map[string]int}`. Implement `tofu.Plan(ctx context.Context, environment config.Environment, manifest release.Manifest, scope *hostlock.Scope) (SavedPlan, error)` and `tofu.Apply(ctx context.Context, environment config.Environment, plan SavedPlan, scope *hostlock.Scope) error` as input adapters to `tofurun.Run`. Keep command execution, explicit child environment, process cancellation, and redaction in configsctl. Pass nil scope for previews and the original acquired scope for deployment. Require both output files and preserve detailed exit status. Implement `preview.Run(ctx context.Context, environment config.Environment, manifest release.Manifest, identity report.Identity) (report.Plan, error)` to start fixed listeners, plan, report, and close listeners.

- [ ] Check out the exact configuration revision and validate the release manifest before OpenTofu execution. Implement the contracts' `access.Start` in `internal/access/access.go` and pin the tested client binary. Start its listeners inside the planning action container before calling the configsctl runner. Read-only previews do not acquire deployment host locks.
- [ ] Use the pinned official artifact action to download the verified artifact ID without extraction and reject a digest mismatch. Run the bounded ZIP/JSON validator before reading release values.
- [ ] Run `preview` for both environments with matching stable labels and restricted credentials. Keep the Access listeners and OpenTofu inside the same action container.
- [ ] Pass the validated environment `Parallelism` value as OpenTofu's `-parallelism` through the configsctl adapter.
- [ ] Publish through `report.Publish` using the qualified plan JSON projection. Keep reporting independent of command execution.
- [ ] Publish checks against the verified PR head, not the protected workflow's commit. Exercise a head change while planning.
- [ ] Show result, change counts, commit, environment, expandable changes, and run links. Preserve earlier results as workflow runs.
- [ ] Keep plan files in ephemeral job storage. Do not upload binary plans or JSON containing sensitive values.
- [ ] Preserve the qualified library cases for create/update/delete, both replacement orders, nested masks, unknown values, precise numbers, output sensitivity, protected addresses, and no-op. Add the demonstrated copied-sensitive-string case to public reporter acceptance. Require the library's decoded masks to match the actual OpenTofu masks and keep all required public change fields after redaction.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestPRPlanReportsNewestCommit -count=1` against a real test PR with two rapid commits. Assert that both commits start plans, the current PR comment identifies the newest commit, and neither plan changes the fixture's managed resources.
- [ ] Commit the workflow and its public-boundary verification.

## Task 3: Select and record releases in merge order

**Files:** Extend `internal/githubapi/client.go` only for shared policy/error handling. Create `internal/queue/select.go`, `internal/queue/reconcile.go`, `internal/deployment/record.go`, and `internal/acceptance/queue_test.go`.

**Interfaces:**

- Define `queue.Request` with `Repository string`, `Set config.DeploymentSet`, and `RunURL string`. Read producer, check, branch, baseline, and environment settings from `Set`.
- Define `queue.Decision` with `State string`, `Commit string`, `PullRequest int`, `ManifestArtifactID int64`, `Reason string`, and `PredecessorReceiptID int64`. Valid states are `ready`, `waiting`, `empty`, and `stopped`.
- Implement `queue.Select(ctx context.Context, client *github.Client, request Request) (Decision, error)` using first-parent commit order and GitHub deployment records.
- Implement `release.Register(ctx context.Context, client *github.Client, repository string, setID string, manifest Manifest) (int64, error)` after provenance verification. Persist the nonsecret manifest before its Actions artifact expires.
- Define `deployment.Attempt` with `SchemaVersion int`, string fields `Repository`, `DeploymentSetID`, `Commit`, `Stage`, `ConfigurationRevision`, `PreviousCommit`, and `RunURL`, plus `ArtifactDigests map[string]string`, `Variables map[string]json.RawMessage`, `RunID int64`, `RunAttempt int`, and `PredecessorReceiptID int64`.
- Use `Stage` values `validation`, `production`, `validation-recovery`, and `production-recovery`. Save these fields in the GitHub deployment payload before starting the corresponding operation.
- Implement `deployment.Begin(ctx context.Context, client *github.Client, attempt Attempt) (int64, error)` and `deployment.Finish(ctx context.Context, client *github.Client, id int64, outcome Outcome) error`. Outcomes use GitHub deployment statuses and preserve failed releases separately from successful recovery attempts.
- Define `deployment.Outcome` with `State` and `Description` strings and `FinishedAt time.Time`. Accept only GitHub deployment states; report recovery through its own attempt rather than changing a failed release to success.
- Define `check.Observation` in `internal/check/types.go` from the contracts now; Task 4 supplies its execution adapter.
- Define `deployment.Receipt` with `Attempt Attempt`, `Result string`, `Validation check.Observation`, and `Production check.Observation`. Implement `deployment.CommitReceipt(ctx context.Context, client *github.Client, receipt Receipt) (int64, error)` with the contracts' terminal outcomes and API options.
- Implement `queue.Reconcile(ctx context.Context, client *github.Client, request Request) (Decision, error)` from commit history and durable receipts.
- Implement `queue.DispatchNext(ctx context.Context, client *github.Client, repository string, workflow string, branch string, setID string) error` using `workflow_dispatch`.
- Use SDK deployment, deployment-status, check, artifact, and workflow-dispatch methods. Keep only the release identity, merge-order, receipt validation, and recovery policy in these packages. Native concurrency supplies exclusion; the selector supplies merge order. Do not build another queue or persistence service.

- [ ] Select the oldest unfinished mainline commit after the verified baseline. Resolve all inputs from that selected commit.
- [ ] Reuse an existing attempt after an uncertain API response. Do not create a second apply because a status request timed out.
- [ ] Distinguish pending builds from terminal failures and expired artifacts using the contracts. Do not lose the release or report it as deployed.
- [ ] Return `stopped` when an incomplete apply or unsuccessful recovery needs investigation.
- [ ] Configure the consumer entry workflow to use one workflow-level concurrency group with `queue: max` and active cancellation disabled. The group must cover selection and both deployment stages.
- [ ] Trigger reconciliation on producer completion, explicit dispatch after a terminal receipt, manual retry, and a 15-minute schedule. Do not depend on token-generated deployment events or indefinite `workflow_run` chains.
- [ ] Set `auto_merge: false` and `auto_inactive: false` on the relevant API requests. Read back exact refs and payloads.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestMergeOrderWithReversedBuildCompletion -count=1`. Assert one verified deployment per commit in merge order for three real merged test PRs.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestReconcileMissingAndDuplicateTriggers -count=1`. Assert recovery of omitted triggers and unchanged apply counts after duplicates. Commit the queue component.

## Task 4: Apply, promote, and recover

**Files:** Create [.github/workflows/deploy.yml](../../../.github/workflows/deploy.yml), `environment/Dockerfile`, `environment/action.yml`, `internal/execution/run.go`, `internal/check/run.go`, `internal/recovery/recovery.go`, and `internal/acceptance/deploy_test.go`. Consume the public configsctl lock package instead of creating a parallel lock implementation.

**Interfaces:**

- Reuse Task 1 configuration and Task 3 decisions. Do not add application-specific branches to the shared workflow.
- Use the contracts' public configsctl `Set`, opaque scope, runner, and restricted transport option. Define `execution.Result{Observed check.Observation, Recovery check.Observation, Result string, AttemptID int64}` and `execution.Run(ctx context.Context, environment config.Environment, candidate release.Manifest, previous release.Manifest) (Result, error)` for the `environment` command. Coordinate only release policy, fixed Access clients, checks, and recovery with standard contexts and `errgroup`; configsctl owns OpenTofu child execution. The outer workflow assembles the final receipt from both environment results. Use the contracts' pinned official runtime rather than another command runner.
- Implement `check.Run(ctx context.Context, definition config.Check, manifest release.Manifest) (Observation, error)` through the Docker Go SDK using the Task 3 observation type and the contracts' mount, credential, output, and deadline restrictions.
- Implement `check.Cleanup(ctx context.Context, definition config.Check, manifest release.Manifest) error` with the same trusted container adapter. A cleanup command returns its exit status rather than a health observation. Run an optional `CleanupCheck` in the normal final path and through the consumer job's standard `always()` cleanup step. The `cleanup` command uses the recorded attempt and protected configuration to invoke this same path after interruption; it does not create another deployment system.
- Define `recovery.Request` with `Environment config.Environment`, `Candidate release.Manifest`, `Previous release.Manifest`, and `Observed check.Observation`.
- Define `recovery.Decision` with `Allowed bool`, `Manifest release.Manifest`, and `Reason string`.
- Implement `recovery.Assess(request Request) (Decision, error)` and `recovery.ValidatePlan(path string, environment config.Environment) error`. Construct recovery from candidate configuration and allowed prior application values. Reject changes to protected resources.

- [ ] Select one release under the complete workflow concurrency exclusion.
- [ ] Start the environment action's fixed Access listeners and acquire the protected host targets before the fresh plan. Follow the contracts' lease timing, first-failure cancellation, and incomplete remote-operation rules. Pass the same opaque scope to configsctl for plan, apply, and recovery; preserve renewal through checks and cleanup.
- [ ] For validation, create a fresh plan for that exact release, report it, and apply that exact saved plan through configsctl. The accepted merge authorizes this apply without comparing against the older PR preview or another approval.
- [ ] Verify installed artifacts and runtime behavior, then record the result.
- [ ] Pass the environment's `Parallelism` value to apply and recovery. Preserve the resource ordering required by the consumer.
- [ ] Execute production only after validation success. Use production's own backend, plan, credentials, and checks. Set each environment job's `timeout-minutes` from its validated protected `JobTimeoutMinutes` and reject credentials that expire before that limit plus fifteen minutes.
- [ ] On a mutation or health failure, inspect actual state, evaluate recovery, plan the previous compatible application values, apply that recovery plan, and check the restored service.
- [ ] Keep original failure and recovery results distinct. Stop the deployment set when recovery fails or actual state cannot be established.
- [ ] Complete owned temporary-task cleanup before releasing host locks. Preserve the original failure if cleanup also fails. A retry confirms that the previous runner ended and cleans its recorded task project before another mutation. Test successful, failed, canceled, repeated, and resumed cleanup with real Compose while preserving unrelated containers and data.
- [ ] Enforce the observation transition table after interruption. An apply error cannot establish that mutation did not occur.
- [ ] Add `TestHostLockSupervisorInteroperates`, `TestHostLockLossCancelsWork`, and `TestCheckUsesAccessWithoutSupervisorSecrets` using real SSH, the unchanged configsctl client, actual child processes, and a Docker check container. Assert mutual exclusion, partial-acquisition cleanup, first-failure cancellation, retained locks for uncertain remote completion, original TLS/host-key validation, and access to only the check's explicit mounts and credentials.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run 'TestPromotion|TestFailedQA|TestCompatibleRecovery|TestFailedRecovery|TestInterruptedApply' -count=1`. Assert the installed version, production mutation count, retained test data, recovery result, and queue state for the relevant scenario.
- [ ] Commit the deployment workflow and recovery behavior.

## Task 5: Prove isolation and publish the reusable workflows

**Files:** Create [.github/workflows/acceptance.yml](../../../.github/workflows/acceptance.yml), fixture configuration under `internal/acceptance/testdata`, and usage documentation in [README.md](../../../README.md).

**Interfaces:**

- Supply a disposable root with the pinned GitHub provider, two independent encrypted remote states, and synthetic release/data files in the explicit test consumer. These actual managed resources persist between GitHub runners.
- Use a dedicated GitHub test consumer with synthetic credentials. Select its repository explicitly through `TOFU_WORKFLOWS_TEST_REPOSITORY`; never default to a production consumer.
- Make the fixture's checker query actual managed release and health fields through GitHub's API. After recovery, assert the previous release and unchanged persistent data. Test the container-check adapter separately against a real HTTP service on the current GitHub runner.

- [ ] Run the complete acceptance matrix from the design using public workflow events and real GitHub APIs. Do not replace GitHub, OpenTofu, Docker, or storage operations with mocks.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestUntrustedPRCannotUseProtectedCredentials -count=1`. Include a synthetic disclosure attempt and assert that protected jobs do not execute that source with privileged credentials.
- [ ] Run `go test -tags=acceptance ./internal/acceptance -run TestSecretValuesAbsentFromPublishedResults -count=1`. Assert that public results and downloadable artifacts do not disclose the fixture's synthetic secret values.
- [ ] Document the consumer contract, required permissions, two-stage configuration, baseline verification, interruption recovery, and immutable references.
- [ ] Publish a reviewed immutable workflow revision only after the real acceptance runs pass. Do not enable a production consumer during library acceptance.
- [ ] Commit the acceptance evidence references and usage documentation.

### Acceptance assertions

Create `internal/acceptance/scenario_test.go` with `runScenario(t *testing.T, name string) Evidence`. It drives actual PRs and workflow runs in the explicit test consumer and reads their comments, check runs, deployment records, job outcomes, and provider-managed resource values. It must not replace those services with test doubles.

Define `Evidence` with `MergedSHAs []string`, `AppliedSHAs []string`, `ApplyCounts map[string]int`, `CurrentCommentSHA string`, `LatestPRHead string`, `ProductionChanged bool`, `ObservedReleaseSHA string`, `PreviousReleaseSHA string`, `DataDigestBefore string`, `DataDigestAfter string`, `QueueState string`, and `SecretFound bool`.

Start each named test with `got := runScenario(t, t.Name())` and use the assertions below. Scenario setup supplies actual commit IDs rather than invented deployment evidence:

```go
// TestPRPlanReportsNewestCommit
if got.CurrentCommentSHA != got.LatestPRHead {
    t.Fatal("the plan comment does not identify the latest PR commit")
}

// TestMergeOrderWithReversedBuildCompletion
if !slices.Equal(got.AppliedSHAs, got.MergedSHAs) {
    t.Fatal("deployment order differs from merge order")
}
for _, commit := range got.MergedSHAs {
    if got.ApplyCounts[commit] != 1 {
        t.Fatal("a release did not apply exactly once")
    }
}

// TestFailedQA
if got.ProductionChanged {
    t.Fatal("production changed after validation failed")
}

// TestCompatibleRecovery
if got.ObservedReleaseSHA != got.PreviousReleaseSHA ||
    got.DataDigestAfter != got.DataDigestBefore {
    t.Fatal("recovery did not restore the application while preserving data")
}

// TestFailedRecovery
if got.QueueState != "stopped" {
    t.Fatal("the queue continued after recovery failed")
}

// TestSecretValuesAbsentFromPublishedResults
if got.SecretFound {
    t.Fatal("published results disclosed a synthetic credential")
}
```

Also implement `TestRegisteredManifestSurvivesArtifactExpiry`, `TestReceiptDoesNotDependOnHistoricalStatuses`, `TestDeploymentUsesExactCommit`, `TestDispatchContinuesQueue`, `TestStateLockDoesNotForceUnlock`, `TestRecoveryPreservesSchemaInputs`, `TestMergeMethodReleaseBoundaries`, and `TestWorkspaceStateIsolation`. Assert the corresponding contract through real workflow and provider results. Obtain failure cases through controlled fixture configuration, canceled test runs, and real artifact deletion in the disposable consumer, not mocked API responses. For the historical-status test, delete synthetic old attempt records after their final receipt exists and prove that queue recovery uses the retained receipt. Do not delete production audit or deployment records.

Run the complete suite with `go test -tags=acceptance ./internal/acceptance -count=1`. Missing test repository or backend credentials must fail explicitly instead of skipping acceptance. Record run URLs, exact tested revisions, and observed resource values before calling a task verified.

## Task 6: Integrate the first consumer separately

Use the separate [Tack implementation plan](2026-10-22-tack-deployment-implementation.md). This task produces verified consumer prerequisites; it does not claim that the generic workflow completes Tack's provider implementation.

Tack is the first consumer. Its [declarative deployment specification](https://github.com/agoodkind/tack/blob/2b1d90ea65519e978ce8e425f3228c9c0542593d/docs/superpowers/specs/2026-10-10-tack-provider.md) requires existing general providers for supported resources and Tack resources only for application-specific behavior.

The inspected merged implementation registers only the database-migration resource. It does not yet supply complete application deployment through OpenTofu. Complete that existing provider work and its integration plan before enabling automatic Tack applies. This plan implements the generic workflow library; it does not silently expand into a replacement provider or full infrastructure migration.

- [ ] Complete the provider and consumer tasks in the Tack implementation plan, including the isolated roots, restricted observation, migration manifest, rollout, and acceptance checks.
- [ ] Replace any proposed SSH-only deployment wrapper with the actual provider authentication and transport. Verify its permissions before enabling live PR planning.
- [ ] Add the small Tack caller workflow at the pinned shared revision after the runtime-audit implementation completes.
- [ ] Verify one QA deployment, production promotion, and compatible recovery with exact installed-image evidence.
- [ ] Keep Configs merge-triggered applies deferred. A pinned configuration revision may be an input without making Configs merges deployment triggers.
