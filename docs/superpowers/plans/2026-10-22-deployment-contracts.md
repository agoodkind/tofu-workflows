# Deployment implementation contracts

Implement the [queued deployment plan](2026-10-22-queued-deployment.md) against these interfaces and state transitions.

## Pin execution dependencies

Use these inspected versions initially. Update a pin only with the corresponding acceptance results.

| Dependency | Revision |
| --- | --- |
| Go | `1.27.3` |
| OpenTofu | `1.13.2` |
| dflook/tofu-plan | `53b5b01b6eef56cd678401dbe92a0f6ca6b863ff` |
| dflook/tofu-apply | `1de081df5087fb9ba2acf4866e9bc9b95f79676c` |
| Both dflook actions' runtime image | `sha256:50305dd21908a624932ffd230fa9919bcd255eaed2f4854e565c31e5a0ea0332` |
| actions/checkout | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| Shared Go CI | `agoodkind/go-makefile` at `bc41cecbc2b2d643d24e426a1ca104a4a4e3350a` |
| GitHub acceptance provider | `integrations/github` version `6.13.0` |

Set `OPENTOFU_VERSION` explicitly. Install providers from the committed lock file and reject a changed lock file after initialization. Disable debug logging and user-supplied pre-run commands in credentialed jobs.

The inspected [plan entry point](https://github.com/dflook/terraform-github-actions/blob/dd5fb128af310eaa8050c9a19e908aa349a404cc/image/entrypoints/plan.sh) passes `-lock=false`. The [apply entry point](https://github.com/dflook/terraform-github-actions/blob/dd5fb128af310eaa8050c9a19e908aa349a404cc/image/entrypoints/apply.sh) accepts `plan_path`, locks during apply, and skips comparison with the PR preview when `auto_approve: true`.

Require a nonempty existing `plan_path`. Pass that exact file to apply in the same job. Never fall back to an implicit plan inside apply. Reject backends that cannot produce a local saved plan.

The [disposable GitHub validation run](https://github.com/agoodkind/tofu-workflows/actions/runs/39814524494) passed checks for plan outputs, sensitive fixture redaction, exact saved-plan apply, and stale-plan rejection at source revision `7d546a5976e526f3dbe0f2e6179fa9d8a1684ab2`. It used only local OpenTofu resources on the GitHub runner. This establishes those upstream action behaviors; the queue, protected-environment integration, and application recovery still require the implementation acceptance tests below.

## Define workspace and release inputs

Distinguish a configuration root with its own backend from a named OpenTofu CLI workspace inside that root. Supply both explicitly. Require the CLI workspace to exist before deployment.

Define these types in `internal/config`:

```go
type Configuration struct {
    Repository string
    Revision string
    Root string
    TofuVersion string
    ProviderLockDigest string
}
type Environment struct {
    Name string
    GitHubPlanEnvironment string
    GitHubApplyEnvironment string
    Configuration Configuration
    CLIWorkspace string
    BackendFile string
    StateID string
    Parallelism int
    VariableFiles []string
    SecretEnvNames []string
    HealthCheck Check
    InspectCheck Check
    RecoveryVariableNames []string
    RecoveryResourceAddresses []string
    ProtectedResourceAddresses []string
}
type Check struct {
    Image string
    Arguments []string
    TimeoutSeconds int
    SecretEnvNames []string
}
type VariableRule struct {
    Kind string
    AllowedValues []string
}
type DeploymentSet struct {
    Version int
    ID string
    SourceBranch string
    InitialCommit string
    ProducerWorkflowID int64
    RecorderActorID int64
    RequiredChecks []CheckIdentity
    DispatchWorkflow string
    ArtifactRepositories map[string]string
    Variables map[string]VariableRule
    Validation Environment
    Production Environment
}
type CheckIdentity struct {
    AppID int64
    Name string
}
```

`StateID` identifies the actual backend and CLI workspace. For S3-compatible state, canonicalize endpoint, bucket, key, and workspace before hashing. Add a tested identity adapter before supporting another backend. Reject duplicate state identities. Require one consumer repository to own each managed state.

Require positive `Parallelism` and pass it to the dflook plan/apply `parallelism` input. This controls concurrent resource operations within an environment and does not replace workflow-level release ordering. Consumers requiring serial resource updates select one.

Accept scalar `string`, `number`, and `bool` release variables. Trusted configuration declares their names, kinds, allowed string values, and artifact repositories. Reject nulls, unexpected keys, executable arguments, configuration revisions, paths, and provider selections from release input.

Define `release.Manifest` with `Version int`, `SourceSHA string`, `BuildRunID int64`, `BuildAttempt int`, `Artifacts map[string]string`, and `Variables map[string]json.RawMessage`. Validate each scalar before writing a JSON variable file. Require `repository@sha256:<64 hex>` image references from configured repositories.

Bind manifests to authenticated GitHub run metadata. Accept exactly one artifact archive entry named `release.json`, at most 64 KiB uncompressed. Reject duplicate entries, symlinks, directories, traversal paths, and trailing JSON documents. Do not trust a manifest's identity fields without API verification.

Resolve a PR preview from its verified producer run and PR head. Require a separate successful build of the exact merged commit for deployment. For each merged PR, use GitHub's final `merge_commit_sha` as the release boundary and order those boundaries by branch ancestry. Intermediate commits from a rebase merge belong to that PR's release. Test merge, squash, and rebase boundaries. Stop on an unassociated direct push rather than inventing PR authorization.

## Protect credentialed jobs

PR builds and tests may publish nonsecret metadata but do not receive deployment credentials. Protected planning runs from the deployment branch through `workflow_run`. Verify repository, numeric producer workflow ID, event, run attempt, head SHA, PR association, and artifact ID through GitHub APIs. Reject forks and unexpected producers in version 1 and publish the failed check.

Execute only the protected configuration revision, pinned providers, pinned shared helper, and protected check images with live credentials. PR metadata supplies typed release values only. Executing arbitrary PR OpenTofu configuration requires a separate isolation design and is outside this initial application-release workflow.

Set `environment` on reusable workflow jobs. Declare the secret `TF_PROVIDER_ENV`, require the caller to pass that name, and check its effective environment-specific value inside the called job. Do not use blanket `secrets: inherit`.

Implement `credentials.Install(raw string, allowed []string, environmentFile string) error`. Decode a JSON map and permit only trusted `SecretEnvNames`. Reject `PATH`, `GITHUB_*`, `ACTIONS_*`, `INPUT_*`, `TF_CLI_ARGS*`, `TF_LOG*`, and `TERRAFORM_PRE_RUN`. Register masking before writing values through the runner environment file. Do not print the map or export it as a job output. Missing values fail before initialization.

Use separate planning and applying identities. Planning credentials must not permit unrestricted commands or resource mutation. Verify this property against each consumer's actual provider transport before enabling live planning.

## Persist deployment outcomes

Use GitHub deployment records in the consumer repository. Selection requires `contents: read`, `actions: read`, `checks: read`, and `deployments: write`. Give `actions: write` only to the final dispatcher. Reporting separately requires `pull-requests: write` and `checks: write`.

Create records with an exact commit `ref`, `auto_merge: false`, explicit `environment`, and `required_contexts: []`. Verify exact required check identities independently before record creation.

Use tasks `tofu-workflows:release`, `tofu-workflows:attempt`, and `tofu-workflows:receipt`. Register each verified merged-release manifest before its build artifact expires. Store its small nonsecret manifest in the release record's immutable payload. Registration does not apply anything. Select the earliest valid registration for a commit. Duplicate identical registrations are harmless; conflicting manifests stop that release.

Versioned attempt and receipt payloads contain repository, deployment-set ID, release SHA, configuration revisions, run ID and attempt, stage, artifact digests, nonsecret release variables, previous verified release, and predecessor receipt ID. Exclude credentials and plan files. Frozen release metadata must remain usable after the original Actions artifact expires.

An attempt records intent before mutation. A receipt records a verified final outcome: `deployed`, `failed_recovered`, or `failed_unmodified`. Create a receipt only when installed releases and both environment outcomes are known. Record the failed release with GitHub's failure status even after successful recovery.

Set `auto_inactive: false`. Store essential recovery information in immutable payloads and the current receipt status. GitHub retains previous status events for 90 days; queue recovery must not depend on those old events. These deployment records do not replace application audit records.

Deduplicate by repository, deployment set, SHA, stage, and run attempt. After an uncertain create response, search for that identity before retrying. Verify API-returned creators against the configured Actions identity. Validate receipt predecessor chains and paginate results; timestamps alone do not establish release order.

## Select and resume work

Use one consumer workflow-level concurrency group, `tofu-workflows-<deployment-set-id>`, with `queue: max` and active cancellation disabled. Do not acquire that group again in a called workflow. The exclusion covers selection, QA, production, and recovery. Select credential environments inside the called workflow's jobs.

Select the first unfinished authorized release after the last verified receipt. Validate first-parent ancestry to `InitialCommit`. Missing baselines, rewritten ancestry, conflicting receipts, and unknown prior outcomes stop that deployment set explicitly.

Read history in pages of 100 and process one release per run. Resume from durable receipt boundaries and cache verified metadata within a run. Exhausted API budgets must defer discovery rather than guess a commit.

Pending builds return `waiting`. A terminal build failure produces an explicit failed result. Inspect the unchanged environments and record `failed_unmodified` before processing a later release. If that inspection fails, stop the set. An expired artifact without a registered manifest requires rebuilding that exact commit; do not select different artifacts or silently mark it deployed. Retrying an older failed release after later deployments requires a new authorized release commit.

After a terminal receipt, use `workflow_dispatch` on the protected consumer branch when another ready release exists. GitHub permits that event with `GITHUB_TOKEN`; ordinary token-generated `deployment_status` events cannot be the continuation mechanism. Producer completion and a 15-minute reconciliation schedule also request work. Do not repeatedly self-dispatch while waiting for builds or while stopped.

Retry transient API reads at most three times and honor `Retry-After`. Read back failed writes before retrying them. Initially allow 10 minutes for inspection and 60 minutes for an environment apply job. Consumers may select larger bounded limits in protected configuration after acceptance establishes the need.

## Apply and recover safely

Within one environment job, generate a plan, require its plan and JSON files, validate changes, report the plan, record intent, and apply the saved `plan_path` with `auto_approve: true`. Keep the same configuration, variables, tool versions, working directory, and backend throughout the attempt. Keep raw plan files in that job.

Retry a `state-locked` failure within the configured bound. Never force-unlock. A failure after apply starts requires inspection; an error does not prove that resources remained unchanged.

Define `check.Observation` with `Healthy bool`, `ReleaseSHA string`, `Artifacts map[string]string`, `StateVersion string`, `PreviousCompatible bool`, and `ReasonCode string`. Accept reason codes `healthy`, `unhealthy`, `unreachable`, `identity-mismatch`, and `incompatible`. Trusted check containers report actual state in this format. Supply only named credential variables and a read-only expected-values file. Do not mount the checkout, Docker socket, or arbitrary host paths into a check container. Limit output to 64 KiB and reject invalid JSON or unknown fields without printing the raw response.

After interruption, select the next operation from actual state:

| Observed state | Required operation |
| --- | --- |
| Candidate is healthy and its artifacts match | Record success without repeating apply. |
| Previous release is healthy and matches the baseline | Record the known failure or recovery outcome without another mutation. |
| State differs and inspection proves compatible recovery | Generate and apply a recovery plan. |
| State is unreadable or compatibility is unknown | Stop the set and preserve the incomplete attempt. |

Construct recovery from the candidate configuration and candidate variables. Replace only `RecoveryVariableNames` with values from the previous verified release. Preserve newer schema-version inputs. Every changed recovery address must appear in `RecoveryResourceAddresses`; reject any change to `ProtectedResourceAddresses`. Replacing an application container may be allowed. Deleting persistent data is not recovery.

Verify health and artifact identities after recovery. Failed recovery stops later releases. Successful recovery records `failed_recovered` before the next release starts. Preserve database changes and audit history.

## Report without races or secret disclosure

The inspected dflook plan action does not publish PR comments for `workflow_run`. Set `add_github_comment: false` and format its outputs with the small Go reporter. Reuse dflook for OpenTofu execution.

Serialize report-producing jobs by repository, PR, and environment; keep other PRs independent. Compare the completed run's head SHA with the current PR head before updating a comment. Update only the configured Actions identity's comment with the exact hidden PR/environment marker. Never edit human comments. Preserve outdated runs without replacing the current comment.

Parse plan sensitivity and unknown-value markers. Suppress sensitive subtrees and outputs. Do not publish raw JSON. Limit displayed changes to 200 lines and each comment to 48 KiB, with a link to the run. Count replacement separately from standalone create and delete actions. Escape untrusted text and strip terminal controls.

Publish checks against the verified PR head SHA, not the protected workflow's `github.sha`. Report errors as failures. Missing counts mean zero only when dflook explicitly returned `changes == false`; otherwise they are an error.

## Test across real workflow runs

Run public-CLI tests with real temporary files and OpenTofu. Workflow acceptance requires `TOFU_WORKFLOWS_TEST_REPOSITORY` and an isolated encrypted remote test backend. Never default these inputs to Tack or Configs.

Use GitHub provider resources for persistent synthetic release records in the test consumer. A trusted check reads those actual resources through the GitHub API. Preserve a second data record while recovering the release record. This proves workflow behavior across ephemeral runners without reserving local compute or claiming Tack runtime coverage.

Test the container-check adapter separately with a real HTTP service on a GitHub runner. Tack requires its own application, migration, health, and recovery acceptance afterward.

Use the primary contracts for [deployment creation](https://docs.github.com/en/rest/deployments/deployments), [status retention](https://docs.github.com/en/rest/deployments/statuses), [token-triggered workflows](https://docs.github.com/en/actions/concepts/security/github_token), and [reusable workflow secrets](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows).
