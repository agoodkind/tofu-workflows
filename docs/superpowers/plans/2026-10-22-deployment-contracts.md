# Deployment implementation contracts

Implement the [queued deployment plan](2026-10-22-queued-deployment.md) against these interfaces and state transitions.

## Pin execution dependencies

Use these pinned versions. Update a pin only with the corresponding acceptance results.

| Dependency | Revision |
| --- | --- |
| Go | `1.27.3` |
| OpenTofu | `1.13.2` |
| OpenTofu Linux amd64 runtime | `ghcr.io/opentofu/opentofu:1.13.2@sha256:912846109c72b5d0ed514653648e6690d0b52f831766561fa605bd6d662cd1b0` |
| configsctl extraction baseline | `26c05c9064ae273657d9d3a666d28f75a6f31711` |
| actions/checkout | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| Shared Go CI | `agoodkind/go-makefile` at `bc41cecbc2b2d643d24e426a1ca104a4a4e3350a` |
| GitHub acceptance provider | `integrations/github` version `6.13.0` |
| Go GitHub SDK | `google/go-github/v93` `v93.0.0`, source `1e7e5ad65e18b07e41636d9559c7d8de29f86576` |
| Artifact download | `actions/download-artifact` `9000827ccba6bdab643e8b6fd33ac0654aef8333` |
| Artifact upload | `actions/upload-artifact` `cf430e030ddbb5b0abf93d22962f4752f3646cd9` |
| Plan JSON types | `hashicorp/terraform-json` `v0.29.0` |
| API retry scheduling | `cenkalti/backoff/v7` `v7.0.1` |
| Process coordination | `golang.org/x/sync` `v0.24.0` |
| Shared workflow Docker client | `github.com/moby/moby/client` `v0.6.2` |
| cloudflared Linux amd64 | `2026.9.1`, SHA-256 `03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc` |

Require `tofu version -json` to match the pinned version before initialization. Install providers from the committed lock file and reject a changed lock file after initialization. Disable debug logging and user-supplied pre-run commands in credentialed jobs.

Use the public configsctl runner defined below. Require a nonempty existing saved plan and apply that exact file in the same job. Never fall back to an implicit plan inside apply. Reject backends that cannot produce a local saved plan.

Use maintained libraries before adding an implementation. The [Go GitHub SDK](https://github.com/google/go-github/tree/1e7e5ad65e18b07e41636d9559c7d8de29f86576) supplies authentication, API types, services, pagination, and typed rate-limit errors. Keep only authorization, safe diagnostics, and bounded retry policy in the workflow adapter. Reuse standard artifact actions, native Actions concurrency, deployment records, Compose, and the existing provider libraries. Add custom code only for a verified requirement those components do not implement; do not add another queue service, state database, task tracker, scheduler, or plugin framework.

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
    JobTimeoutMinutes int
    AccessTunnels []AccessTunnel
    HostLocks []HostLockTarget
    VariableFiles []string
    BackendSecretEnvNames []string
    SecretEnvNames []string
    SecretFiles map[string]string
    HealthCheck Check
    InspectCheck Check
    CleanupCheck *Check
    RecoveryVariableNames []string
    RecoveryResourceAddresses []string
    ProtectedResourceAddresses []string
}
type Check struct {
    Image string
    Arguments []string
    TimeoutSeconds int
    SecretEnvNames []string
    SecretFileNames []string
}
type AccessTunnel struct {
    ID string
    Hostname string
    LocalPort int
    ClientIDEnv string
    ClientSecretEnv string
}
type HostLockTarget struct {
    ID string
    Address string
    ConnectAddress string
    User string
    PrivateKeyFile string
    KnownHostsFile string
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

Require positive `Parallelism` and pass it to OpenTofu plan/apply through configsctl. This controls concurrent resource operations within an environment and does not replace workflow-level release ordering. Consumers requiring serial resource updates select one.

## Reuse configsctl execution

Extract the existing `executeTofu`, redaction writer, and process-group handling into public `goodkind.io/configsctl/tofurun`. Expose the existing lock implementation as `goodkind.io/configsctl/hostlock`. Preserve current local CLI behavior. The shared helper links these packages; it does not invoke dflook entrypoints or copy their command runner.

Define these interfaces in configsctl:

```go
type OutputMode string
const (
    RedactedProgress OutputMode = "redacted_progress"
    PrivateJSON OutputMode = "private_json"
)
type Request struct {
    Binary, Root, Workspace string
    Args, Environment, RequiredEnvironment, SecretEnvironment []string
    SecretFiles map[string]string
    OutputMode OutputMode
    JSONPath string
    RequiredHosts []string
}
type Result struct { ExitCode int; LogPath string }
func Run(ctx context.Context, request Request, scope *hostlock.Scope) (Result, error)
```

`Binary` selects `/usr/local/bin/tofu` in the pinned image. Resolve `Root` beneath the validated checkout and require the declared existing CLI workspace. Initialize with `-input=false -lockfile=readonly` and the selected backend file, then select that workspace without creating it. Compare the provider lock digest after initialization. Plan with `-input=false -no-color -detailed-exitcode -parallelism=N -out=<private-plan>` and the declared variable files. Only read-only previews add `-lock=false`; deployment plans and applies retain backend locking. Apply with `-input=false -parallelism=N <exact-saved-plan>`. Saved-plan mode does not prompt or replan. Require a nonempty regular file, privately copy it before inspection, and use that same copy for apply. Preserve statuses zero, one, two, and crash status eleven.

Construct `Environment` from the protected allowlist and ordinary `PATH`, `HOME`, `TMPDIR`, `LANG`, and `TF_IN_AUTOMATION` settings. Require `TF_IN_AUTOMATION=1` and a job-private HOME/TMPDIR. Never append the parent environment. Exclude GitHub and Access credentials, infrastructure-vault unlock credentials, unrelated management tokens, `TF_CLI_ARGS*`, `TF_LOG*`, `INPUT_*`, and pre-run hooks. Register declared secret values and exact `SecretFiles` contents with the existing redactor before starting a child. `SecretFiles` maps each allowed variable name to its installed protected pathname; require the matching environment assignment. Reject duplicate assignments, missing credentials, unmaskable secret values, secret-bearing command arguments, or file paths outside the private credential directory. Do not read the Configs vault or run Ansible inventory in this API.

`RedactedProgress` streams redacted stdout and stderr into Actions and the existing private human-readable log. `PrivateJSON` permits only `show -json <saved-plan>`, creates `JSONPath` exclusively under the private job directory with mode 0600, and writes raw stdout only to that file. Redact stderr; do not change JSON bytes by string substitution. The reporter reads the private file and publishes only the permitted projection. Remove temporary plans and JSON after the job; never upload them.

Add `(*hostlock.Set).Scope() (*Scope, error)`. Keep `Scope` fields private and bind it to the creating process, run, controller, active Set, and acquired host identities. Before mutation, `Run` rejects a nil, foreign-process, canceled, closed, expired, or incomplete-target scope. `CONFIGS_LOCK_STATE` does not authorize a bypass. Read-only preview uses a nil scope; only its nonmutating init, existing-workspace selection, plan, show, validate, and format checks are permitted. The environment action retains the original Set through checks, recovery, and cleanup and alone renews or releases it. Do not reacquire a shorter lease inside the runner.

Add `configsctl ci tofu --config <file> --root <root> --workspace <workspace> -- <tofu argv>` as the read-only CLI adapter. Its strict JSON configuration contains `version: 1`, `required_environment`, `secret_environment`, `secret_files`, `output_mode`, and optional `json_path`. Environment lists contain allowed variable names, not values; resolve only those names plus the fixed ordinary process settings. `PrivateJSON` requires `json_path`. Reject duplicate/unknown fields and mutation commands. Dispatch `ci` before existing vault initialization. The shared workflow uses `tofurun.Run` with an acquired scope for mutation rather than adding a lock-bypass CLI flag.

Use `signal.NotifyContext` for SIGINT/SIGTERM at CLI and workflow entrypoints and the existing `procgroup.RunGroup` twenty-second termination grace. Keep the independent cleanup context and uncertain remote-operation rules below. A process exit alone does not prove guest completion.

Build both action images from the pinned official OpenTofu Linux amd64 image above, replace its entrypoint with the compiled helper, and copy only the trusted helper and verified cloudflared binary. Use the existing shared Go build for the helper. Pin the completed configsctl module revision after its acceptance passes; the extraction baseline is not an implemented API revision.

## Define release manifests

Accept scalar `string`, `number`, and `bool` release variables. Trusted configuration declares their names, kinds, allowed string values, and artifact repositories. Reject nulls, unexpected keys, executable arguments, configuration revisions, paths, and provider selections from release input.

Define `release.Manifest` with `Version int`, `SourceSHA string`, `BuildRunID int64`, `BuildAttempt int`, `Artifacts map[string]string`, and `Variables map[string]json.RawMessage`. Validate each scalar before writing a JSON variable file. Require `repository@sha256:<64 hex>` image references from configured repositories.

Bind manifests to authenticated GitHub run metadata. Download the verified artifact ID with the pinned official action using `repository`, `run-id`, `artifact-ids`, `skip-decompress: true`, and `digest-mismatch: error`. Its [action contract](https://github.com/actions/download-artifact/blob/9000827ccba6bdab643e8b6fd33ac0654aef8333/action.yml) supports preserving the archive for validation. Use Go's ZIP/JSON libraries to accept exactly one archive entry named `release.json`, at most 64 KiB uncompressed. Reject duplicate entries, symlinks, directories, traversal paths, and trailing JSON documents. Do not trust a manifest's identity fields without API verification.

Resolve a PR preview from its verified producer run and PR head. Require a separate successful build of the exact merged commit for deployment. For each merged PR, use GitHub's final `merge_commit_sha` as the release boundary and order those boundaries by branch ancestry. Intermediate commits from a rebase merge belong to that PR's release. Test merge, squash, and rebase boundaries. Stop on an unassociated direct push rather than inventing PR authorization.

## Protect credentialed jobs

PR builds and tests may publish nonsecret metadata but do not receive deployment credentials. Protected planning runs from the deployment branch through `workflow_run`. Verify repository, numeric producer workflow ID, event, run attempt, head SHA, PR association, and artifact ID through GitHub APIs. Reject forks and unexpected producers in version 1 and publish the failed check.

Execute only the protected configuration revision, pinned providers, pinned shared helper, and protected check images with live credentials. PR metadata supplies typed release values only. Executing arbitrary PR OpenTofu configuration requires a separate isolation design and is outside this initial application-release workflow.

Set `environment` on reusable workflow jobs. Declare `TF_BACKEND_ENV` and `TF_PROVIDER_ENV` separately. The backend bundle contains only state-storage credentials and the environment's state-encryption passphrase. The provider bundle contains only provider authentication. Require explicit caller bindings and check each effective environment-specific value inside the called job. Reject overlapping variable names between the bundles. Do not use blanket `secrets: inherit`.

Implement `credentials.Install(raw string, allowed []string, files map[string]string, environmentFile string) error`. Install the backend bundle against `BackendSecretEnvNames` and the provider bundle against `SecretEnvNames`. Decode each JSON map and reject undeclared keys. Reject `PATH`, `GITHUB_*`, `ACTIONS_*`, `INPUT_*`, `TF_CLI_ARGS*`, `TF_LOG*`, and `TERRAFORM_PRE_RUN`. Register masking before writing values through the runner environment file. Encode multiline values with a fresh delimiter that does not occur in the value. Do not print the map or export it as a job output. Missing values fail before initialization.

Use `SecretFiles` to map an allowed variable to its protected job-temporary pathname. Write that value into a mode-0600 file under a mode-0700 directory and export only its pathname under the original variable name. Use this binding for private SSH keys. Require paths beneath the dedicated runner-temporary credential directory, reject existing/symlink targets, and remove the files after the action. Keep the provider's configured key-file references consistent with these paths. Test the actual key file, permissions, cleanup, and absence of key bytes from the environment file.

Use separate planning and applying identities. Planning credentials must not permit unrestricted commands or resource mutation. Verify this property against each consumer's actual provider transport before enabling live planning.

## Connect through fixed Access endpoints

Use Cloudflare Access TCP forwarding for consumers with private endpoints. Each protected `AccessTunnel` selects one hostname and a unique loopback port. Cloudflare Tunnel configuration fixes the origin; requests cannot select another destination. Provision exact-service-token Access policies before publishing DNS. Keep planning and applying service identities separate, and permit each only on its declared applications.

Run a pinned `cloudflared access tcp` subprocess inside the same action container as OpenTofu. Bind only `127.0.0.1` on the configured port. Supply the two named credential variables only to that subprocess and stop it during cleanup. Do not give the runner a tunnel management token, a zone-management token, or private-network enrollment credentials. Require successful authorized connection within thirty seconds before planning or applying.

Download the pinned client from its [official release asset](https://github.com/cloudflare/cloudflared/releases/download/2026.9.1/cloudflared-linux-amd64) and verify the recorded SHA-256 before including it in the action image. The pinned Linux image uses this architecture; another architecture requires its own verified asset.

Preserve endpoint identity independently from the socket address. HTTPS clients retain the original URL hostname and verify its certificate while dialing the local listener. SSH clients verify the original target's pinned host key while dialing the local listener. Never disable verification or select a different User-Agent to evade an HTTP challenge.

Define `access.Start(ctx context.Context, tunnels []config.AccessTunnel) (*Session, error)` and `(*Session).Close() error` in `internal/access`. Validate hostname, unique port, and allowed credential-variable references before starting any process. Return an error if a listener exits during the operation. Keep protected provider connection settings consistent with these fixed ports; release input cannot change them.

Trusted check and cleanup containers share the supervisor's network namespace to use the same loopback listeners. Pass only their explicit credential allowlist and expected-values file. A `SecretFileNames` entry may select one file created by `SecretFiles`; mount that exact file read-only and pass its pathname. Reject undeclared files and directory mounts. Do not mount the supervisor's Docker socket or configuration checkout. Resolve each bind's Docker-host path from the supervisor's own mount metadata; a container-local pathname does not identify a host file. GitHub's [container-action implementation](https://github.com/actions/runner/blob/main/src/Runner.Worker/Handlers/ContainerActionHandler.cs) supplies the supervisor's Docker socket and workspace mounts.

## Acquire compatible host locks

An OpenTofu backend lock excludes other users of that state. It does not exclude an infrastructure or guest deployment using different state. Consumers with existing host locks must configure every affected guest and hypervisor in `HostLocks`. The shared action must acquire them through configsctl before the fresh deployment plan.

Install an administrator-owned fixed Go command in configsctl, `host-lock serve --principal ID`. SSH configuration supplies the principal and fixed command, disables shell/file transfer/forwarding, and permits only the protected host. Accept one JSON request of at most 4 KiB with `protocol_version`, `operation`, `run_id`, and `lease_seconds`. Accept `acquire`, `renew`, `release`, and `extend`; reject duplicate or unknown fields and caller-selected paths. Permit acquire/renew leases from one through 4200 seconds and extend requests from one through 900 seconds. Prefix stored run identities with the server's principal. The caller cannot renew or release another principal's lock.

Reuse the [existing configsctl lock protocol](https://github.com/agoodkind/configsctl/blob/26c05c9064ae273657d9d3a666d28f75a6f31711/internal/hostlock/lock.sh): the fixed lock file, boot-specific atomic directory guard, three-field record, expiry rules, and matching-owner release. Reject symlink paths, malformed records, and nonempty stale guards without deleting their contents. Return bounded status codes plus the host time and expiry.

Expose the existing configsctl `hostlock` package for external consumers and extend its existing `Set`, `AcquireAll`, `RenewAll`, `KeepRenewed`, and release implementation. Do not build a second `LeaseSet`. Add `OperationTransport.Do(ctx context.Context, host Host, request Request) (Response, error)`, with the existing typed operation/run/controller/lease fields and expiry/host-time reply. Add `AcquireAllWithOptions(ctx context.Context, hosts []Host, run, controller string, lease Lease, options Options) (*Set, error)`, where `Options` selects the transport, strict renewal failure, and per-request timeout. Preserve the old default API for existing callers. Add a checked release result for callers that must verify completion. Keep fixed-path filesystem operations and restricted SSH inside configsctl, not the shared workflow. Reuse its partial acquisition cleanup and renewal machinery, and publish/pin this extension before linking it into the workflow.

Define `Request` with `Operation Operation`, `Run, Controller string`, and `Lease time.Duration`. Define `Response` with `HostTime, ExpiresAt time.Time`, and `Options` with `Transport OperationTransport`, `StrictRenewal bool`, and `RequestTimeout time.Duration`. `Operation` uses the four protocol values above. Retain the existing `Host` fields and add `ConnectAddress`, `PrivateKeyFile`, and `KnownHostsFile` strings for the restricted SSH transport. Its endpoint ignores caller-selected lock directories. Implement `(*Set).ReleaseAllChecked(ctx context.Context) error` and `(*Set).Abandon()`; abandon stops renewal and invalidates scopes without removing remote lock records. Preserve the old release method for existing callers. An acquired empty target set may produce a valid scope for consumers without host locks; backend locking still applies.

Use one environment-execution Docker action containing the pinned OpenTofu executable, cloudflared, and compiled workflow helper. Link the helper to the completed configsctl public runner and lock packages. Run plan, reporting, recorded intent, apply, checks, recovery, and cleanup within one lock lifetime. Keep process execution in configsctl and deployment policy in the workflow. Do not add another supervisor, executable shim, deployment controller, or shell script.

Acquire and renew 4200-second leases every sixty seconds. Bound each renewal request to ten seconds. Cancel supervised work on the first failed renewal. Tack's existing migration command permits 3600 seconds, plus thirty seconds of polling margin and five seconds for remote termination. It fits inside the 4130 seconds remaining after a failed renewal. Keep the legacy client's defaults unchanged. Its 900-second cap applies only to extend, and its renew preserves a later expiry. Never share the supervisor's run identity with a legacy acquire at a shorter duration, which could shorten that expiry. Reject a configured remote operation that exceeds the lease budget before execution.

Hold host locks from the fresh deployment plan through apply, verification, recovery, and task cleanup. A local child-process exit does not prove that remote work stopped. Run an optional protected `CleanupCheck` on every exit with an independent bounded context. Cleanup reports success only after removing its owned temporary resources. Supply a standard GitHub Actions final cleanup step as a fallback. That step reacquires only the same attempt identity and full lease after the main action terminates; a conflicting owner prevents cleanup. Derive the run identity from repository, workflow run ID, run attempt, and environment. A resumed attempt cleans the ended runner's temporary project under the same locks before another mutation. Preserve existing deployment receipts; do not add a separate task tracker. Release early only after remote completion and cleanup; otherwise leave the recorded lease to expire and inspect before the next mutation. Committed graceful shutdown may continue completing admitted requests after expiry. Inspect its phase and refuse another replacement until that shutdown finishes. Read-only PR previews do not run mutating cleanup or acquire deployment host locks.

Test the complete supervisor with real SSH, filesystem locks, child processes, and shortened deadlines. Assert mutual exclusion with the unchanged client, renewal, partial acquisition cleanup, expiry, principal separation, wrong-owner release, cancellation, and incomplete remote work. Never force-unlock.

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

Use SDK error types and the established backoff library for at most three total transient read attempts, honoring `Retry-After` and reset times. Do not retry writes, applies, or recovery indiscriminately. Read back uncertain writes by durable identity before retrying. Allow 10 minutes for inspection. Require protected `JobTimeoutMinutes` from one through 180 and use it as the reusable job's `timeout-minutes`. Use 60 for generic acceptance and 120 for Tack, which must permit its existing one-hour migration plus downloads, checks, and recovery. The selector must validate this value before publishing the job definition. Credential validity must exceed the full configured job limit by at least fifteen minutes.

## Apply and recover safely

Within one environment job, generate a plan, require its plan and JSON files, validate changes, report the plan, record intent, and apply the exact saved plan through configsctl. Keep the same configuration, variables, tool versions, working directory, and backend throughout the attempt. Keep raw plan files in that job.

Set OpenTofu's `-lock-timeout=300s` for deployment plans, applies, and recovery. After that bound, report the lock failure without another implicit apply or force-unlock. A failure after apply starts requires inspection; an error does not prove that resources remained unchanged.

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

Publish results with the small Go reporter. Reuse configsctl for OpenTofu execution and the qualified JSON library for action counts and selected changes.

Serialize report-producing jobs by repository, PR, and environment; keep other PRs independent. Compare the completed run's head SHA with the current PR head before updating a comment. Update only the configured Actions identity's comment with the exact hidden PR/environment marker. Never edit human comments. Preserve outdated runs without replacing the current comment.

Decode supported plan fields with `terraform-json`, enabling `Plan.UseJSONNumber(true)` before decoding and validating the format. Publish only the selected change projection, never the full plan, variables, or prior state. Suppress marked sensitive subtrees and outputs. Collect string leaves at marked before/after and output paths, then mask matching strings in the display projection before formatting. Real OpenTofu output can copy such a value into an unmarked field; the library preserves the supplied masks but does not provide disclosure policy. Preserve unknown markers, limit displayed changes to 200 lines and each comment to 48 KiB, and link to the run. Count replacement separately from standalone create and delete actions. Escape untrusted text and strip terminal controls. Fail on an unsupported format rather than guessing its meaning.

Publish checks against the verified PR head SHA, not the protected workflow's `github.sha`. Report errors as failures. OpenTofu plan exit zero means unchanged and exit two means changed. Require a valid saved plan and JSON in either case; another exit or missing output means failure. Count supported actions from that JSON and reject unsupported actions.

## Test across real workflow runs

Run public-CLI tests with real temporary files and OpenTofu. Workflow acceptance requires `TOFU_WORKFLOWS_TEST_REPOSITORY` and an isolated encrypted remote test backend. Never default these inputs to Tack or Configs.

Use GitHub provider resources for persistent synthetic release records in the test consumer. A trusted check reads those actual resources through the GitHub API. Preserve a second data record while recovering the release record. This proves workflow behavior across ephemeral runners without reserving local compute or claiming Tack runtime coverage.

Test the container-check adapter separately with a real HTTP service on a GitHub runner. Tack requires its own application, migration, health, and recovery acceptance afterward.

Use the primary contracts for [deployment creation](https://docs.github.com/en/rest/deployments/deployments), [status retention](https://docs.github.com/en/rest/deployments/statuses), [token-triggered workflows](https://docs.github.com/en/actions/concepts/security/github_token), and [reusable workflow secrets](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows).
