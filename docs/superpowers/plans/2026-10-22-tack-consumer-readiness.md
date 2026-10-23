# Tack consumer-readiness plan

> Use `subagent-driven-development` for implementation and review. Track the steps below before enabling the shared workflow for Tack.

**Goal:** Establish that a Tack release can use the shared workflow without applying unrelated infrastructure or exposing production credentials.

**Architecture:** Keep Tack application resources in isolated OpenTofu roots and state. Reuse the established provider ownership from the existing Tack specification. Supply the shared workflow with exact configuration revisions, release manifests, protected credentials, and real application checks.

**Tech stack:** OpenTofu, the existing Tack and general providers, Configs, and the shared GitHub Actions workflows.

**Spec:** Follow the [shared design](../specs/2026-10-22-queued-deployment-design.md) and the existing [declarative Tack specification](https://github.com/agoodkind/tack/blob/2b1d90ea65519e978ce8e425f3228c9c0542593d/docs/superpowers/specs/2026-10-10-tack-provider.md).

## Global constraints

- Create a dedicated Tack application deployment configuration with separate state for QA and production. It manages application releases and migrations.
- Continue managing servers and shared guest setup through the existing infrastructure and guest workspaces.
- Do not apply the whole Configs infrastructure or guest state to deploy a Tack application release.
- Keep Configs merge-triggered deployment deferred.
- Preserve existing resource identities, data volumes, database versions, and audit history during adoption.
- Complete runtime auditing before enabling automatic Tack deployment.
- Allow a planned restart outage of at most 60 seconds when the deployment configures one application instance. The user reports that the current deployment has one instance.
- Preserve service availability during deployments with multiple configured application instances. Update one instance at a time and verify its readiness before updating the next. An unhealthy instance does not permit the deployment to use the single-instance outage exception.
- Do not introduce a second deployment command outside managed provider resources.

## Verified prerequisites and gaps

The inspected Configs revision is `8f70434225fd2e26bf75c519134a7ab4320dcd24`. It defines separate state files for infrastructure, guest configuration, and firmware. Its command wrapper calls these independent configuration roots workspaces.

The existing roots have these responsibilities:

| Root | State file | Relevant boundary |
| --- | --- | --- |
| Infrastructure | `opentofu.tfstate` | Includes Tack guests together with other guests, networking, and Cloudflare resources. |
| Guest configuration | `guest.tfstate` | Includes shared guest preparation and several Tack QA guests. |
| Firmware | `firmware.tfstate` | Manages hardware firmware configuration. |

The inspected Tack revision `9efb81e0f8e5cf22e8710be773af0344f0901f6d` registers only `tack_schema`. That resource manages migrations. Ansible still installs application images. Neither finding establishes a complete Tack application workspace.

The provider also executes guest commands through the Proxmox API. A token that permits that operation does not establish a read-only planning identity. Resolve the actual permissions before enabling live PR plans.

## Reuse provider capabilities

Source inspection establishes the following application-release mapping. It does not establish installed provider versions or successful deployment through these resources.

| Operation | Existing implementation | Required integration or correction |
| --- | --- | --- |
| Download the exact release image | `kreuzwerker/docker` provides `docker_image`, including digest references and retained local images. | Declare server and consumer digests from the release manifest. Download images before interrupting service and preserve the previous release for recovery. |
| Read and replace application containers | `docker_container` manages image, command, mounts, ports, restart behavior, health checks, and import by container ID. | Define one owner for each application container. Preserve existing networks and volumes. Compare an adoption plan before allowing replacement. |
| Connect to guest Docker | `pveguest-docker-socket` already exposes a local Unix socket backed by Proxmox guest execution. | Reuse this transport where compatible. Verify actual image pulls, inspection, replacement, and health polling against its buffered responses and command deadlines. |
| Apply database migrations | Tack provides `tack_schema` with live version reads, pending migration reporting, import, guarded apply, and post-apply verification. | Separate candidate migration metadata from the currently installed Compose image. Keep the newer schema version during application recovery. |
| Verify the installed release | `ops deploy verify` compares server and consumer image digests. `/healthz` checks FoundationDB and YugabyteDB. | Accept explicit instance and service targets instead of requiring both fixed container names on every guest. Check required functional behavior and audit consumption separately. |
| Preserve availability across instances | Docker can wait for one container to become healthy. Tack performs HTTP shutdown with a ten-second deadline. | Coordinate removal from request routing, request completion, replacement, health verification, and return to routing for one instance at a time. Single-container health waiting does not implement this sequence. |

Extend `kreuzwerker/docker`'s `docker_compose` resource for application container state, as approved by the user. Preserve Compose as the single configuration source. Add individual planned service settings, live drift detection, and import that preserves existing containers, networks, and volumes. Do not introduce duplicate Tack image or container resources or separately maintained container declarations. Verify transport and adoption before selecting the provider build for deployment.

The existing Docker bridge is not a credential boundary. Its `-get-only` option filters HTTP methods inside the client process, which still reads a Proxmox guest-execution token. It forwards permitted response bodies without field filtering. Restricting methods alone does not restrict access to container inspection data. Its guest requests have a 60-second command timeout and reject events and attached streams; long image pulls need explicit compatibility testing or a bounded transport correction before adoption.

The inspected `docker_compose` implementation checks whether project containers exist during `Read`, while its planned content changes use a file hash. It does not implement import or compare individual live service fields. The approved extension must correct these gaps through resource state and lifecycle behavior, rather than a command wrapper.

The current migration reader runs `docker compose run --pull never tack-ops migrate --output json`. It starts a temporary container and reads migrations from the installed image. A new release can contain migrations that image does not include. Extend `tack_schema` to consume candidate migration metadata bound to the release digest, compare it with the live database version during planning, and execute that same candidate during apply. Preserve its expected-version check and verify the resulting version. Do not install the candidate before PR planning merely to discover its migrations.

The Docker container reader also omits environment and label refresh. Full drift reporting for those fields requires a general provider correction. Passing passwords through its ordinary `env` configuration would put them in OpenTofu resource state; use protected runtime secret references under the separate credential design.

Source evidence:

- [Docker image resource](https://github.com/kreuzwerker/terraform-provider-docker/blob/f5b5f8c4aa7dd333d36f6cc5a4327d316a7d2e73/docs/resources/image.md) and [container resource](https://github.com/kreuzwerker/terraform-provider-docker/blob/f5b5f8c4aa7dd333d36f6cc5a4327d316a7d2e73/docs/resources/container.md) define the existing lifecycle inputs and import behavior.
- [Docker container refresh](https://github.com/kreuzwerker/terraform-provider-docker/blob/f5b5f8c4aa7dd333d36f6cc5a4327d316a7d2e73/internal/provider/resource_docker_container_funcs.go#L889) omits environment and label refresh. [Compose resource implementation](https://github.com/kreuzwerker/terraform-provider-docker/blob/f5b5f8c4aa7dd333d36f6cc5a4327d316a7d2e73/internal/provider/resource_docker_compose.go#L158) checks project presence and hashes input files.
- [Guest Docker bridge](https://github.com/agoodkind/terraform-provider-pveguest/blob/a1b623c7b6f9969033e5a91408a69cbb26917e62/cmd/pveguest-docker-socket/proxy.go) implements method filtering, buffered responses, and request limits.
- [Tack migration commands](https://github.com/agoodkind/tack/blob/9efb81e0f8e5cf22e8710be773af0344f0901f6d/provider/internal/tackprovider/command.go) select the installed Compose image. [Schema apply](https://github.com/agoodkind/tack/blob/9efb81e0f8e5cf22e8710be773af0344f0901f6d/provider/internal/tackprovider/schema_apply.go) checks the planned image and verifies the final database version.
- [Release verification](https://github.com/agoodkind/tack/blob/9efb81e0f8e5cf22e8710be773af0344f0901f6d/internal/ops/deploy_verify.go) requires fixed server and consumer container names. [Health probes](https://github.com/agoodkind/tack/blob/9efb81e0f8e5cf22e8710be773af0344f0901f6d/internal/runtime/graph_http.go) check the two databases. [Server shutdown](https://github.com/agoodkind/tack/blob/9efb81e0f8e5cf22e8710be773af0344f0901f6d/cmd/server/server_runtime.go) allows ten seconds for HTTP shutdown.

## Review focus

- An application plan includes infrastructure resources: reject adoption before automatic apply.
- Import proposes replacement of an existing data object: stop and correct ownership without applying.
- A planning identity can execute arbitrary guest commands: withhold live planning until the transport enforces the intended boundary.
- Recovery lowers the desired schema version: reject the recovery plan.
- A generic workflow test passes: still require Tack's own installed-image, runtime, and audit evidence.

## Task 1: Establish application ownership

- [ ] Read the existing Tack provider implementation and Configs deployment sources at their current revisions.
- [ ] Map every application deployment field to the general provider or Tack-specific resource that owns it under the existing specification.
- [ ] Complete the separate provider implementation plan for the missing resources. Supply its exact resource schemas, live readers, mutation behavior, imports, and acceptance tests before implementing them.
- [ ] Define independent application roots and backend keys for QA and production. Follow Configs' existing independent-root convention. Keep shared guest and host resources in their current states.
- [ ] Verify import with complete plans and explicit resource addresses. Accept adoption only when it preserves live resources and avoids duplicate writers.

This provider implementation is existing unfinished work, not a generic workflow feature. The shared library can proceed while its owner completes these prerequisites.

## Task 2: Establish provider access and state protection

- [ ] Inventory the exact API operations used by each provider during read, plan, apply, and recovery from source and disposable-environment execution.
- [ ] Supply a metadata-only planning identity and a separate applying identity. Prove that the planning identity rejects mutation and unrestricted guest execution.
- [ ] Reuse encrypted state and saved-plan protection from the current OpenTofu configuration. Store encryption and backend credentials in protected GitHub environments; never copy decrypted values into documents or workflow outputs.
- [ ] Verify that required host or deployment locks remain effective when GitHub invokes OpenTofu. Do not bypass locks currently enforced by Configs tooling.
- [ ] Record exact root paths, backend identities, provider versions, credential variable names, and permission-test results as the consumer configuration. Do not substitute broad credentials when a required restriction fails.

## Task 3: Connect and validate the consumer

- [ ] Generate a manifest for the exact merged Tack commit with server and audit-consumer image digests and required migration inputs.
- [ ] Configure the shared workflow using the verified application roots and immutable provider lock files.
- [ ] Implement trusted health and inspection commands that return the shared observation contract from real running services.
- [ ] Test a normal release, a QA failure, a production health failure, compatible recovery, and interrupted apply on disposable infrastructure.
- [ ] Verify a single-instance release restores service within 60 seconds after service interruption begins. Verify a multiple-instance release serves requests throughout the update and waits for each updated instance to become ready before updating another. Verify an unhealthy instance prevents an update that would interrupt service.
- [ ] Verify exact installed images and runtime audit records. Only then enable automatic production deployment after the audit work completes.

## Planning status

The shared workflow plan supplies its implementation contracts. This companion identifies the remaining Tack-specific work; it is not a completed implementation plan for the missing providers or their permission boundary. Do not report the complete Tack rollout as de-risked until those concrete plans and acceptance results exist.
