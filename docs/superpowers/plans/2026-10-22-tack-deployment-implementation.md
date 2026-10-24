# Tack application deployment implementation plan

> Use `subagent-driven-development` for implementation. Track completion with checkbox syntax and review each task before integrating dependent changes.

**Goal:** Plan Tack releases from protected live metadata and deploy merged releases through QA and production with the approved availability and recovery behavior.

**Architecture:** Extend the general Docker Compose resource, existing guest transport, and Tack schema resource. Compose remains the application configuration source. A restricted SSH reader supplies planning observations; existing providers perform changes. Separate application states and serial applies isolate each environment.

**Tech stack:** Go, OpenTofu, Terraform Plugin Framework, Compose Go packages, Docker Engine, OpenSSH, Traefik, and GitHub Actions.

**Spec:** Read the [queued design](../specs/2026-10-22-queued-deployment-design.md), [shared contracts](2026-10-22-deployment-contracts.md), [approved consumer decisions](2026-10-22-tack-consumer-readiness.md), and [disposable results](2026-10-22-tack-deployment-spikes.md).

## Global constraints

- Complete runtime auditing before enabling automatic Tack deployment. Preserve audited operator reads and changes.
- Extend `docker_compose`; preserve Compose as the single configuration source. Do not add duplicate Tack container resources.
- Limit application resources to selected services and migrations. Preserve existing guest, database, network, volume, search, and secret-file ownership.
- Keep credentials and secret contents out of state, plans, logs, and observation responses. Use protected runtime file references.
- Use the restricted planning login without Docker mutation access or a Proxmox execution token. Authenticate apply separately.
- Preserve the approved timing and availability requirements in the consumer decisions. Download images before interruption; promote immediately after QA passes.
- Apply each isolated application root with `parallelism=1` and serialize replicas inside each Compose resource.
- Keep Configs merge-triggered automation deferred. Recovery changes application images without reversing database changes.
- Treat disposable prototypes as evidence, not production implementation. Complete the real acceptance cases before enabling deployment.

## Review focus

- A permitted target contains secret fields: explicit response conversion must exclude them, including diagnostics.
- A Compose project contains database services: selected-service operations must preserve those services and their data.
- A provider stops during request preparation: lease expiry restores readiness before commitment, while committed shutdown must finish admitted requests.
- A failed command has already changed an image or database: observe actual state before recovery.
- A prototype passes locally: prove the completed provider, audit integration, backend permissions, and installed configuration separately.

## Task dependencies and ownership

Task 1 supplies observation to Task 3. Tasks 2 and 4 can proceed independently. Task 5 supplies request preparation to Task 3. Task 6 adopts completed resources. Task 7 connects the shared workflow after integration acceptance.

Paths below are repository-relative implementation destinations. New files are planned interfaces, not claims of existing implementation.

| Repository | Create or modify | Responsibility |
| --- | --- | --- |
| Guest provider | `deploymeta/{protocol,policy,docker,ssh}.go`, `cmd/pveguest-deployment-metadata/main.go` | Restricted observation protocol and client. |
| Guest provider | Existing Docker socket proxy; `dockerrequest/request.go`, `cmd/pveguest-docker-request/main.go` | Bounded remote image downloads. |
| Docker provider | Existing Compose resource and focused `compose_{model,observation,import,rollout}.go` extensions | Nested state, import, and serial service updates. |
| Tack | `internal/releasemeta/{manifest,verify}.go`, `cmd/migration-manifest/main.go`, schema provider files | Candidate migration catalog and guarded apply. |
| Tack | `provider/internal/tackprovider/compose_cleanup.go`, `provider/cmd/tack-deploy-cleanup/main.go` | A small companion command for the existing guest transport to run scoped Compose cleanup. |
| Tack | `internal/deploycontrol/{controller,socket,http}.go`, `internal/ops/deploy_{control,inspect}.go`, release verifier | Audited observation, request preparation, and verification. |
| Configs | New application roots/module, existing SSH configuration, Traefik routes, `configsctl.yml` | State isolation, protected accounts, routing, and locks. |
| Shared workflow and Tack | Environment contract and consumer workflow | Exact release inputs and complete acceptance. |

## Task 1: Implement restricted observation

Publish the protocol in the guest provider's public `deploymeta` package and pin its revision in the Docker provider and Tack. Use explicit structures rather than raw Docker responses.

```go
type Request struct {
    ProtocolVersion int
    Target string
}
type Observation struct {
    ProtocolVersion int
    Target string
    ObservedAt time.Time
    Project string
    Services map[string]ServiceFields
    Instances map[string]Instance
    Schema *SchemaObservation
    AuditOperationID string
}
type SchemaObservation struct {
    Present bool
    AppliedVersion int64
}
type Instance struct {
    ContainerID, ImageID, ImageDigest, Status, Health, Generation string
    CreatedAt time.Time
}
type SSHConfig struct {
    Address, User, PrivateKeyFile, KnownHostsFile, Target string
    ConnectAddress string
}
type Policy struct { Version int; Targets map[string]TargetPolicy }
type TargetPolicy struct {
    Project string
    Services, PublicEnvironment, PublicLabels []string
    PublicCommand, PublicHealthCommand, SchemaEnabled bool
}
```

Use snake_case JSON keys, protocol version 1, and RFC3339 UTC timestamps. Define `ServiceFields` in this same public `deploymeta` package using Task 3's field specification; the guest provider must not import the Docker provider. Both maps use stable `<service>/<replica-number>` keys, allowing the observation to represent different settings across replicas. Exclude one-off containers and reject duplicate identities. A target is an administrator-owned logical mapping to a project and permitted services, never a caller-selected path, endpoint, or command.

Implement `Read(ctx context.Context, request Request, policy Policy, source Source) (Observation, error)`, `ReadSSH(ctx context.Context, connection SSHConfig) (Observation, error)`, and `Run(ctx context.Context, input io.Reader, output io.Writer, policy Policy, source Source) error`. `Source.Observe(ctx context.Context, target TargetPolicy) (Observation, error)` reads approved fields. Verify response version, target, and freshness; use a thirty-second observation deadline and reject timestamps outside the request interval plus five seconds of clock tolerance.

- [ ] Reject unknown or duplicate request fields, unsupported versions, trailing objects, and input over 4 KiB. Bound output to 1 MiB. A missing required observation fails planning instead of producing guessed defaults.
- [ ] Return permitted nonsecret settings only. Omit health output, log text, file contents, unrestricted labels/environment, and command fields unless policy explicitly permits them. Return bounded error codes without raw Docker or SQL diagnostics.
- [ ] Read Docker metadata through the existing Moby SDK. Use `golang.org/x/crypto/ssh` and `ssh/knownhosts.New` for SSH and host-key verification. Keep custom code limited to target policy, explicit field projection, audit admission, bounds, and the separate connection address; do not implement another Docker HTTP client or SSH protocol.
- [ ] Add `ops deploy inspect` through Tack's existing audited read mechanism. Reuse the `migrate --output json` status reader without `--execute` for the applied version. Require strict audit admission and return the durable operation UUID as `AuditOperationID`. Reject paths that skip audit recording. Fail before disclosing metadata when audit admission or completion fails. Record allowed and denied requests under the server-configured planning identity, never an identity supplied by request JSON. Expose the generic protocol without embedding Tack types in the guest provider.
- [ ] Install an administrator-owned fixed executable, policy, authorized-key file, and parent directories. Configure the dedicated account with a fixed command, disabled forwarding/TTY/user startup commands/password login, and rejected file transfers. The planning account must not modify these files or inject environment settings. The fixed process may read its explicitly configured local dependencies; it must not expose them to the caller.
- [ ] Add a provider `observation` block using `SSHConfig`. Plan jobs instantiate only the restricted reader. Create, Update, and Delete fail without separately supplied mutation credentials. Apply re-reads live state before mutation. Configure host-key verification and protected key-file references. When `ConnectAddress` selects a loopback Access listener, verify the key against the original `Address`; never trust a newly scanned proxy key.
- [ ] Add `TestRestrictedPlanningReadsRealDocker` with real SSH, Docker, and the audited Tack source. Assert secret exclusion, ownership/version/target validation, request bounds, denied shell/SFTP/forwarding, denied policy writes, and audit events. Assert audit-storage failure prevents metadata disclosure and a valid response's operation UUID identifies its completed audit event. Use synthetic secret values and inspect only test-owned containers.
- [ ] Run `make build` in each owning repository and `go test ./deploymeta ./cmd/pveguest-deployment-metadata -count=1` through its configured integration environment. Run Tack's existing container integration target for the audited source. Commit protocol, reader, and account installation as reviewable units.

## Task 2: Bound guest image downloads

In `dockerrequest/request.go`, define `DownloadPolicy{WarnAfter, Timeout time.Duration}`, `PullRequest{Image, ExpectedDigest string}`, and `PullResult{ImageID, Digest string, Duration time.Duration}`. Implement `Pull(ctx context.Context, request PullRequest, policy DownloadPolicy) (PullResult, error)`. The guest command accepts one typed pull request through stdin and uses the fixed local Docker socket.

Extract the existing proxy request implementation into the guest provider's public `dockertransport` package. Define `Options{GuestSocket string, MaxBodyBytes int64, Download dockerrequest.DownloadPolicy}` and `New(client *transport.Client, guest transport.Guest, options Options) (http.RoundTripper, error)`. Retain the existing socket executable as a caller of that package. The Docker provider's apply connection can use the same implementation directly without a persistent proxy process or another controller container.

- [ ] Use the existing Moby client 0.5.0 `ImagePullResponse.Wait` or `JSONMessages` API in the guest helper. Preserve SDK errors and return a bounded final result. Verify the expected digest after completion. Add only the warning timer and deadline; do not write another progress JSON parser or buffer progress into the Proxmox stdout limit. Keep this helper dependency separate from the Compose provider's older Docker client.
- [ ] Extend `transport.NodeConfig` with optional `ConnectAddress`. Keep `Endpoint` as the HTTPS URL and TLS identity. Override only the dial address, require a protected loopback endpoint, and retain normal certificate validation. Map the provider's `connect_address` input to that field. Use the shared action's fixed TCP listener; the existing Go client must retain its normal User-Agent.
- [ ] Extend the existing Docker socket proxy's image-pull operation to use the helper. Propagate the configured ten-minute timeout into remote execution and retain the existing thirty-second polling margin. Reject invalid durations. Keep metadata requests separately bounded.
- [ ] Emit one thirty-second warning with safe image identity and elapsed time. Configure outer workflow/provider deadlines to exceed the download deadline. Inspect image state after uncertain cancellation without interpreting a timeout as proof that nothing downloaded.
- [ ] Finish all required downloads before migration or request preparation. Preserve the previous verified image and keep application container IDs unchanged after a download failure.
- [ ] Add `TestGuestPullWarningAndTimeout` and `TestGuestPullLargeProgressResponse` using a disposable Proxmox guest and real registry. Assert bounded output, error preservation, digest verification, cancellation, warnings, and unchanged running containers. Use a real request longer than sixty seconds and shortened configurable cancellation cases.
- [ ] Run `make build` and `go test ./dockerrequest ./cmd/pveguest-docker-socket -count=1`, followed by the guest acceptance target with an explicit disposable guest. Commit the helper and transport extension together.

## Task 3: Extend Docker Compose planning and import

Keep the existing Compose inputs. Add `managed_services set(string)`, `observation_target string`, and optional `rollout` settings from Task 5. Tack requires an explicit nonempty service selection and `remove_orphans=false`. Extend the existing provider's `loadComposeProject`; use Compose Go's loader and `Project.WithSelectedServices` with `IgnoreDependencies`. Reuse the library's merge, default, and path handling. Selected application operations must disable dependency startup.

Add a computed `services` map keyed by `<service>/<replica-number>`. `ModifyPlan` supplies desired settings for each declared replica; `Read` supplies actual settings for each observed replica. Keep current status and IDs in a separate computed `instances` map. Mark new/replaced instance identity unknown during planning. Do not let routine health changes create configuration updates.

`ServiceFields` contains these typed fields:

| Field | Type | Rule |
| --- | --- | --- |
| `Image` | `string` | Compare immutable digest identities. |
| `Command`, `Entrypoint` | `[]string` | Preserve order and explicit empty values; require policy permission. |
| `User`, `WorkingDir`, `Hostname` | `string` | Separate declared values, image defaults, and generated hostnames. |
| `Environment`, `Labels` | `map[string]string` | Compare approved nonsecret values; exclude generated Compose labels. |
| `Restart` | `RestartPolicy{Name string, MaximumRetryCount int64}` | Normalize the documented Docker default. |
| `Healthcheck` | `Healthcheck{Test []string, IntervalNS, TimeoutNS, StartPeriodNS, StartIntervalNS, Retries int64, Disabled bool}` | Compare configuration, never health output. |
| `Mounts` | `map[string]Mount` keyed by destination | `Mount{Type, Source, Propagation string, ReadOnly, CreateHostPath bool}`. Preserve named-volume identities. |
| `Ports` | `[]Port` | `Port{Target int64, Published, HostIP, Protocol string}`. Distinguish automatic allocation from declared ports. |
| `Networks` | `map[string]Network` | `Network{Aliases []string, IPv4Address, IPv6Address string}`. Compare only declared static addresses. |
| `ExtraHosts` | `map[string]string` | Compare declared hostname/address entries, including Tack's ledger peers. |
| `NetworkMode`, `StopSignal` | `string` | Compare explicit host networking and shutdown signal settings. |
| `DNS`, `DNSOptions`, `DNSSearch` | `[]string` | Preserve resolver order where meaningful. |
| `Logging` | `Logging{Driver string, Options map[string]string}` | Compare public log-driver settings without returning log contents. |
| `StopGracePeriodNS` | `int64` | Compare the configured graceful-stop period. |
| `Limits` | `Limits{MemoryBytes, MemorySwapBytes, NanoCPUs, PIDs int64}` | Normalize units. |
| `Security` | `Security{ReadOnly, Privileged bool, CapAdd, CapDrop, SecurityOpt []string}` | Compare each declared setting. |

Use pointers or explicit presence flags when absence differs from zero. Compare replica membership through the stable map keys. Reject unsupported configured fields before a successful plan instead of reducing them to a checksum.

Define `ComposeSnapshot{Project string, Services map[string]deploymeta.ServiceFields, Instances map[string]deploymeta.Instance}` and `ComposeImportIdentity{Project string, Services []string}`. Implement:

```go
NormalizeComposeProject(project *composetypes.Project, selected []string,
    imageDefaults map[string]deploymeta.ServiceFields) (map[string]deploymeta.ServiceFields, error)
ReadComposeSnapshot(ctx context.Context, source ComposeMetadataSource,
    identity ComposeImportIdentity) (ComposeSnapshot, error)
ParseComposeImportID(value string) (ComposeImportIdentity, error)
```

`ComposeMetadataSource.ReadProject(ctx context.Context, identity ComposeImportIdentity) (ComposeSnapshot, error)` selects an approved observation target and verifies returned project/service identity. Restricted SSH and apply-time Docker implementations use the same normalized model. Never fabricate missing raw Docker fields to imitate a complete API response.

Implement `ReadImageDefaults(ctx context.Context, imageReference string) (deploymeta.ServiceFields, error)` using the existing containerd registry resolver, OCI descriptors, digest and platform packages. Fetch bounded manifest/configuration metadata, verify its digest, and apply the disclosure policy. Planning may use registry-read permission but must not install or run that image on the guest. Reuse library authentication and digest handling.

Resolve an OCI index to the Docker host's declared operating system and architecture, verify the selected manifest and configuration digests, and compare that chain with the observed container image. An index digest, platform-manifest digest, and image configuration ID identify different objects. Reject ambiguous platform selection or an unverified chain rather than comparing unlike digests as strings. Cover both index and single-platform references in Compose acceptance.

- [ ] Populate the same typed nested Terraform Plugin Framework attributes from desired and observed settings. Use Framework equality, `ResourceWithModifyPlan`, and `ResourceWithImportState` to produce normal OpenTofu field differences. Do not add a parallel diff engine. Preserve state on observation failure and distinguish absent objects from unreadable objects. Use explicit SDK projection for live metadata; Compose Generate skips inspection errors, combines replicas, and copies unrestricted environment values at the inspected version.
- [ ] Add an optional `pveguest` connection block to the Docker provider with the existing node/guest identity and protected token-file reference. Construct its Docker HTTP client through Task 2's shared RoundTripper. Keep native Docker host configuration available for other consumers. Planning uses only the observation block and must not load apply credentials. Test both modes with identical normalized output.
- [ ] Implement import ID `<project>/<sorted-service1,service2>` without file access or remote mutation. Populate identity and observed fields. The next configured plan must accept file paths without recreating matching containers. Reject duplicates and ambiguous project identities.
- [ ] Reuse Compose Service Create, Start, Ps, Remove, and existing convergence for selected services. Preserve unselected services, external networks, volumes, and guest files. Add only the per-instance preparation/readiness integration that Compose's concurrency setting cannot provide. Do not build another container reconciler or use whole-project Down against application resources.
- [ ] Preserve absolute guest `secrets.file` and `configs.file` references. Correct the misleading client-side existence warning for remote Docker. Never download guest secrets. Verify required daemon-side paths without reading contents before replacement and set `CreateHostPath=false`.
- [ ] Use Task 2 to download images and Task 5 to update availability-sensitive replicas. Serialize replicas internally, complete readiness/routing restoration before returning, and retain prior image digests for recovery.
- [ ] Add public OpenTofu tests `TestAccComposeImportPreservesObjects`, `TestAccComposeDetectsRestartDrift`, `TestAccComposeServiceSelectionPreservesData`, `TestAccComposeReportsFieldChanges`, `TestAccComposeRemoteFileSecret`, and `TestAccComposeMetadataOnlyPlan`. Assert exact changed fields, stable data identities, zero-mutation matching import, and secret absence from plans/state/diagnostics.
- [ ] Run `make build` and `TF_ACC=1 go test ./internal/provider -run TestAccCompose -count=1` against disposable Docker. Publish an immutable tested build and commit provider lock checksums. Keep the extension generic and contribute through the upstream repository's normal workflow.

## Task 4: Plan migrations from the proposed release

Reuse `schemastatus.Available(ctx, migrations.FS)`, which already enumerates ordered version/name/checksum entries without a database. Create `internal/releasemeta/manifest.go` with `Manifest{Version int, SourceSHA, ImageDigest string, Migrations []schemastatus.Migration}`. The existing exported `schemastatus.Migration` type contains `Version int64`, `Name string`, and `SHA256 string`.

Implement `Build(ctx context.Context, sourceSHA, imageDigest string, files fs.FS) (Manifest, error)`, `Decode(input []byte) (Manifest, error)`, and `Verify(ctx context.Context, manifest Manifest, files fs.FS) error`.

- [ ] Add an offline catalog command before database configuration loads. The build must use the same embedded SQL as the image. Reject duplicate versions and invalid catalogs. Attach the final image digest externally after publication; do not embed the image's own final digest inside it.
- [ ] Publish `migration_manifest` as a bounded JSON string in the shared release manifest's existing scalar variables. The protected consumer parses typed entries and checks source SHA, verified build, and server digest. Preserve the existing total manifest size bound.
- [ ] Add `candidate_image string`, `candidate_commit string`, and `candidate_migrations list(object({version=number,name=string,sha256=string}))` to `tack_schema`. Retain its import identity. Plan pending migrations from candidate metadata and the restricted live applied-version read, without installing or executing candidate code.
- [ ] Add optional `api_token_file` and `connect_address` to Tack's provider node model and schema. Require exactly one token source only when configuring the mutation client; read the protected token file inside that client and never replace its path with token bytes in configuration or state. Map `connect_address` to Task 2's transport field. Observation-only planning must not load either mutation token source.
- [ ] Add ordered `compose_files list(string)` for the schema execution, using the same protected base and environment override as the application. Generate a typed YAML override containing only the migration service's verified candidate image and nonsecret execution labels. Submit that override through transport stdin and a final `-f -` argument, with the declared install directory as the project directory. Compose supports [ordered files and stdin](https://docs.docker.com/reference/cli/docker/compose/). Do not change the installed image, copy secrets, or substitute a tag into the base file to select the candidate.
- [ ] Recheck starting database version before apply. Verify the candidate digest and embedded catalog, then use the existing audited guarded migration with `--expect-from` and `--to`. Verify the resulting database version before application updates.
- [ ] Preserve the existing 3600-second migration limit from the schema provider's `applyTimeoutSeconds`. Add an absolute RFC3339 `--deadline` to the migration command and pass it from the protected provider. Cancel the migration's Go context on that deadline or SIGTERM. Reject expired deadlines before mutation. Give post-cancellation version observation and audit completion a separate thirty-second context that preserves the operation identity. Bound the full command, including its terminal audit write, to the deadline plus thirty seconds.
- [ ] Run the existing migration service with `--rm --no-deps -T --pull never` in a separate Compose project for this attempt. Derive the project from the schema identity, candidate source SHA, and target version. Preserve the declared Compose files, install directory, and enabled `ops` profile. Keep application services and their data outside this project.
- [ ] Register standard cleanup before starting the task. Run `docker compose` with those same files, project, and `--profile ops`, followed by `down --timeout 60 --remove-orphans`. Run cleanup after success, failure, and cancellation through a bounded context independent of the canceled work. Never pass `--volumes`, `-v`, or `--rmi`, and never run `down` against the application project. Confirm that the task project's containers and networks no longer exist.
- [ ] Implement `CleanupMigration(ctx context.Context, client *transport.Client, request MigrationCleanup) error` in the existing provider package. `MigrationCleanup` contains `Guest transport.Guest`, `InstallDir, Project, CandidateImage string`, `ExecutionLabels map[string]string`, and ordered `ComposeFiles, Profiles []string`. Reuse the resource's typed image/label override builder so cleanup supplies the same final stdin file as execution. Validate the protected task-project prefix and use the existing `transport.Client.Run` with fixed Compose argv. Reuse this function in the resource's final cleanup and a companion `tack-deploy-cleanup` command packaged with the provider's trusted tool image. The command decodes protected configuration and the recorded release identity, loads only its declared provider credential, and exits after cleanup. It does not implement deployment, scheduling, or another task state store.
- [ ] Use the deployment action's ordinary final cleanup path as a second opportunity after provider interruption. On a later retry, confirm from existing GitHub attempt records that the previous runner ended, acquire the existing host locks, and clean its task project before reading the database version and starting another migration. Fail the attempt if cleanup cannot confirm removal. Reuse Docker's project labels and existing deployment records; do not add a migration tracker, scheduler, or observation API.
- [ ] Preserve candidate schema inputs during application recovery. Permit only application image variables in recovery; the older binary must pass compatibility tests against the newer schema.
- [ ] Add `TestCandidateManifestDeterministic`, `TestCandidateManifestRejectsChangedSQL`, `TestSchemaPlansUninstalledCandidate`, `TestSchemaRefusesChangedStartingVersion`, and `TestApplicationRecoveryPreservesNewSchema`. Use real YugabyteDB and old/new images for runtime cases.
- [ ] Add `TestMigrationCleanupAfterClientLoss` through the real migration CLI, Docker Compose, and YugabyteDB with a shortened deadline and deliberately slow synthetic SQL. Terminate the client, run the normal cleanup path, and verify task removal, query cancellation, database version, and audit outcome. Repeat cleanup and clean an interrupted attempt before retry. Assert that the original application container ID, process, network, volumes, and synthetic data remain unchanged and that a cleanup failure prevents another migration.
- [ ] Run `make build`, the existing Tack container integration target, and provider acceptance. Commit catalog publishing and provider integration separately.

## Task 5: Complete requests before replacement

Create `Controller`, `DrainRequest`, and `DrainStatus` in `internal/deploycontrol/controller.go`. `DrainRequest` contains `ReleaseID`, `InstanceID`, `OwnerToken`, and `ExpectedGeneration` strings. `DrainStatus` contains `Generation string`, `Phase string`, `ActiveRequests int64`, `LeaseExpiresAt time.Time`, and `ErrorCode string`. Phases are `serving`, `preparing`, `committed`, and `stopped`.

Implement `NewController(instanceID, generation string) *Controller`, `Track(next http.Handler) http.Handler`, `Begin(request DrainRequest) (DrainStatus, error)`, `Renew(request DrainRequest) (DrainStatus, error)`, `Resume(request DrainRequest) (DrainStatus, error)`, `Commit(request DrainRequest) (DrainStatus, error)`, and `Status() DrainStatus`. A process generation changes on restart. Count application requests through response completion, preserve streaming interfaces, and exclude health/control requests.

- [ ] Serve bounded control JSON on a mode-0600 Unix socket at `/run/tack/deploy-control.sock`. Expose begin/renew/resume/commit/status through the existing audited Go `ops deploy` command family. Reject conflicting owners and stale generations; make current-owner retries idempotent.
- [ ] Add `/readyz` from dependency health and preparation state. Preserve `/healthz`. Preparation withdraws readiness but continues processing already dispatched requests.
- [ ] Use an engineering-selected fifteen-second preparation lease renewed every five seconds. Expiry restores readiness only before commitment. Committed shutdown never resumes automatically.
- [ ] Add a generic rollout adapter with `RolloutTarget{Project, Service, InstanceID, ContainerID, ProxyService, ProxyUpstream string, ControlArgv []string}` and `PreparedTarget{Target RolloutTarget, OwnerToken, Generation string, ExcludedAt time.Time}`. Implement `Prepare(ctx context.Context, target RolloutTarget) (PreparedTarget, error)`, `Commit(ctx context.Context, target PreparedTarget) error`, and `Restore(ctx context.Context, target PreparedTarget) error`. Execute fixed administrator-configured argv without a shell; keep application names out of generic code. For Proxmox guests, call the existing guest transport with `docker exec` and the fixed container/argv rather than attempting an unsupported hijacked Docker Exec connection through the buffered HTTP bridge. Capture only the bounded control response. Poll process exit with short inspection requests rather than a long attached stream.
- [ ] Implement `ProxyObserver.BackendStatus(ctx context.Context, serviceID, upstreamURL string) (string, error)` using authenticated Traefik metadata. Require a ready peer, begin preparation, and confirm proxy DOWN within fifteen seconds. Start the approved thirty-second request deadline after confirmed exclusion.
- [ ] Before commitment, cancel and restore if requests remain, peers fail, exclusion fails, or lease renewal fails. Verify proxy UP before reporting restoration. Preserve the original container and active requests.
- [ ] Commit after exclusion and zero active requests. Disable lease restoration, begin graceful HTTP shutdown, and wait for requests admitted during the final transition. Do not force-kill requests. Report an incomplete operation if the provider deadline expires; a later apply observes phase, generation, process exit, and image before recovery.
- [ ] Recreate only after confirmed process exit. Verify image, readiness, and proxy UP before another resource or replica updates. Single-instance updates skip peer preparation and retain the approved outage budget.
- [ ] Enumerate declared application endpoints in Traefik and change its probe to `/readyz` after application support exists. Retain the current ten-second interval and three-second timeout. Observe the protected API; do not expose an insecure production API.
- [ ] Add `TestRollingUpdatePreservesRequestsHTTP1AndH2C`, `TestDrainTimeoutRestoresWithoutStoppingContainer`, `TestDrainLeaseExpiryRestoresReadiness`, `TestStaleDrainOwnerCannotResumeNewGeneration`, `TestProxyExclusionFailureCancelsBeforeReplacement`, and `TestLateRequestCompletesDuringCommit`. Use actual container replacement, real requests/dependencies/audits, concurrent POST IDs, and provider interruption. Assert duplicate/missing operations separately without client POST retries.
- [ ] Run the existing Tack integration harness and Compose public acceptance tests. Commit application control and generic provider orchestration separately.

## Task 6: Configure and adopt isolated state

Create application declarations against these independent backends:

| Configs root | Bucket | Key | CLI workspace |
| --- | --- | --- | --- |
| `opentofu/tack-qa` | `tofu-state-tack-qa` | `tack-qa.tfstate` | `default` |
| `opentofu/tack-prod` | `tofu-state-tack-prod` | `tack-prod.tfstate` | `default` |

- [ ] Create backend, encryption, versions, providers, variables, main, and outputs `.tf` files in each root. Reuse the existing R2 endpoint/S3 options, `use_lockfile=true`, `pbkdf2.state`, `aes_gcm.state`, and input name `vault_tofu_state_passphrase`. Enforce state and plan encryption. Supply each environment's independent encryption key and bucket credential through `TF_BACKEND_ENV`, without reading or copying the infrastructure vault. Set QA `workspace_key_prefix="tack-qa-workspaces"` and production `workspace_key_prefix="tack-prod-workspaces"` inside their respective buckets. Use the read-only credential for previews and the applying credential for state writes and locking.
- [ ] Create `opentofu/modules/tack-application` with HCL inputs `compose_files list(string)`, `project string`, `managed_services set(string)`, `observation_target string`, `server_digest string`, `consumer_digest string`, and Task 5 rollout inputs. Use pinned environment instance maps. Keep service settings in Compose.
- [ ] Declare an explicit Docker provider alias and module block for each configured application guest. Pass that alias to the shared application module; use stable module addresses such as `module.application_primary.docker_compose.application`. Do not use a module `for_each` that attempts to choose provider aliases dynamically. Use `tack_schema.application` for the migration owner and make application modules depend on schema completion. Preserve pre-existing networks and data volumes as external resources.
- [ ] Pin the initial application map to QA CT 217 (`app`, `audit-consumer`), QA CT 223 (`app`), and production CT 117 (`app`, `audit-consumer`). Refresh these observations under the handoff locks. Define any additional instance in a later declaration and normal deployment plan.
- [ ] Render only nonsecret interpolation values from pinned inventory into the runner's Compose input. Do not copy the rendered guest `.env`. Retain the existing ledger peer mapping. Validate the final merged Compose model for conflicting inline/file credentials before planning.
- [ ] Add exact new-root host/deployment lock targets in `configsctl.yml` and the protected shared-workflow configuration. Include each affected guest and hypervisor. Use the contracts' public configsctl runner and fixed Go lock endpoint with explicit CI inputs; keep the application-state key separate from the local infrastructure vault. Set Tack `Parallelism` to one. Maintain complete release exclusion across QA and production.
- [ ] Normalize credential delivery through the existing deployment owner before import. Reuse the separately reviewed runtime-file provisioning work; do not duplicate its generator or add shell scripts. Preserve existing credential values and place only each service's required files on its guest. Correct the inline `DATABASE_URL` override to the current `DATABASE_URL_FILE` contract. Scope the existing deployment to selected application services with dependency startup disabled. Validate this real configuration change on QA before production, within the approved availability limits. Require healthy applications, working audits, individual protected mounts, absent inline secret variables, and unchanged persistent stores.
- [ ] Install Task 1's trusted strict audited reader before schema observation. Add only the fixed status operation to the reviewed operator command allowlist and forbid `--execute` on that read path. Do not run candidate PR code or direct SQL with production credentials to obtain it.
- [ ] Acquire the same host locks, capture normalized live identities, and disable the old application writer for selected services only. Preserve its ownership of guest files, secret provisioning, infrastructure, and unrelated services. Keep the locks through the ownership handoff; do not permit two active application writers.
- [ ] Import the normalized selected services and observed schema version. Require an adoption plan without remote changes and compare container IDs, creation times, networks, volumes, and mounts. Do not conceal old inline credentials with placeholders or `ignore_changes`. Transfer an already-owned resource between states explicitly instead of importing it twice; do not copy a whole infrastructure state. Enable the new writer only after the matching plan and ownership tests pass.
- [ ] Add `TestApplicationRootsAreIndependent` and `TestEncryptedApplicationStateLocking` with real disposable S3 storage. Assert encryption, wrong-key rejection, independent keys/lineages, lock denial, normal cancellation release, and unchanged production objects after QA operations.
- [ ] Verify the completed workflow against its actual bucket, state key, encryption key, and role credentials before adoption. Do not print/decrypt existing production state for validation.
- [ ] Run `tofu fmt -check` and `tofu validate` through the configsctl CI runner in the protected workflow, then provider adoption acceptance. Application-state operations receive the GitHub environment's independent key explicitly rather than reading the infrastructure vault. Commit application declarations and exact ownership changes. Keep unrelated Configs automation deferred.

## Task 7: Integrate the shared workflow

Create `internal/releasemeta/compatibility.go` with `CompatibilityPair{CandidateSHA, CandidateServerDigest, CandidateConsumerDigest, PreviousServerDigest, PreviousConsumerDigest, MigrationManifestSHA256 string, SchemaBefore, SchemaAfter int64}` and `CompatibilityEvidence{Pair CompatibilityPair, BuildRunID int64, BuildAttempt int, Passed bool}`. Implement `CheckCompatibility(pair CompatibilityPair, evidence CompatibilityEvidence) error`. Construct the expected pair from verified release receipts, the candidate manifest, and actual schema observation. Require every field to match and a successful result from the authenticated test workflow. After migration, require the actual applied version to equal `SchemaAfter` before setting `PreviousCompatible=true`. A healthy endpoint alone cannot establish compatibility.

The compatibility workflow must run both immutable application images against real disposable stores. Create synthetic records with the previous image, apply the candidate migrations, exercise the candidate, then restart the previous image against the upgraded stores and repeat read/write and audit checks. Preserve the synthetic records and audit chain. Publish the evidence only after every phase passes. Use a GitHub-hosted runner; do not reserve shared QA or use production data.

Before an environment changes, select the prior image from its last verified deployment receipt and match that exact pair to the authenticated compatibility evidence. If an earlier failed release changed the required pair, dispatch the same disposable test automatically with the actual prior image. A failed compatibility test records a failed unmodified release. Missing or pending evidence waits for its specific test run rather than accepting an unverified rollback. This verification does not add an operator approval.

- [ ] Extend Tack's existing build workflow to publish exact server/consumer digests and migration metadata for PR and merged commits. Reuse the shared artifact provenance checks and protected configuration revisions.
- [ ] Configure separate QA/prod planning and apply environments. Install restricted planning key/host-key references only in planning environments. Keep mutation and backend-write credentials out of PR build jobs.
- [ ] Set protected `JobTimeoutMinutes` to 120 for both Tack environments. Preserve the existing one-hour migration limit and allow the remaining job time for image downloads, checks, and recovery. Require backend credentials to remain valid for the whole job plus fifteen minutes before starting it.
- [ ] Provision fixed Access endpoints for every configured planning guest, lock endpoint, and apply API. Use separate environment and role service identities with exact-token policies. Keep the connector origins and provider loopback bindings in protected configuration. Install its pinned client in the shared action container. Verify the completed provider over those endpoints, not only the disposable connection tester.
- [ ] Configure separate planning and applying credentials for each environment's state bucket. Generate the independent encryption passphrases specified in the consumer prerequisites and install the protected backend bundles. Verify permitted and denied bucket operations and secret delivery before enabling previews or state adoption. Keep credentials and passphrases out of plans, logs, artifacts, and operator output.
- [ ] Configure the shared workflow's optional `CleanupCheck` with `TimeoutSeconds=120` to invoke that provider companion command with only the declared applying credential and protected task target. Use its standard final cleanup step and existing deployment receipts for retries. This bound includes the sixty-second Compose stop, guest polling, and termination margin. Do not supply backend credentials, application data, or service passwords to cleanup.
- [ ] Extend `ops deploy verify` with `ExpectedInstance{Target, Service, InstanceID, Digest string}` and `VerifyInstances(ctx context.Context, expected []ExpectedInstance) (Verification, error)`. `Verification` contains `Instances []InstanceResult` and `Healthy bool`; `InstanceResult` contains target, service, instance ID, expected/actual digest, health, and error-code strings. Fail for missing, duplicate, stale, incorrect-image, or unhealthy declared instances.
- [ ] Verify application readiness through the configured `/readyz` endpoint and audit-consumer readiness through its declared check. Do not equate a missing Docker healthcheck with a failed application, or a running container with a working service. Keep readiness endpoints in protected configuration, not caller-selected URLs.
- [ ] Adapt the result to shared `check.Observation`. Derive compatibility from tested release metadata and actual schema, not HTTP health. Retain candidate migration inputs during application-image recovery.
- [ ] Add `TestCompatibilityEvidenceMatchesActualPreviousRelease` through real workflow artifacts and disposable old/new images. Assert that another previous digest, migration checksum, source SHA, failed test, or incorrect applied version cannot set `PreviousCompatible=true`. Assert that automatic verification of the actual pair permits deployment without another approval.
- [ ] Run fresh release, adoption, drift, wrong image, download failure, migration failure, QA failure, production failure, compatible recovery, interrupted apply, and failed-recovery scenarios through real workflows. Assert exact images, schema, persistent synthetic data, audit events, and queue order.
- [ ] Keep QA functional tests separate from per-instance verification. Promote immediately after required QA success. Validate the approved outage and multiple-instance request behavior before merging deployable changes.
- [ ] Establish one verified initial release after audit completion, normalization, import, and full acceptance. If QA and production still differ, apply and verify that release through QA and then production before enabling queue discovery. Set `InitialCommit` to that exact merged source and create the baseline receipt from both verified environments. Never infer a common baseline from differing image tags.
- [ ] Enable the immutable shared workflow revision. Record merge, workflow run, installed digests, and observed behavior as separate evidence.

## Completion criteria

Implementation is complete only after the named public-boundary acceptance tests pass. Production readiness additionally requires the actual guest transport, protected account configuration, backend permissions, and installed application checks.
