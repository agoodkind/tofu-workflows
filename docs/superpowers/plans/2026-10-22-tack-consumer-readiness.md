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
- Allow a planned restart outage of at most 60 seconds when the deployment configures one application instance.
- Preserve service availability during deployments with multiple configured application instances. Update one instance at a time and verify its readiness before updating the next. An unhealthy instance does not permit the deployment to use the single-instance outage exception.
- Do not introduce a second deployment command outside managed provider resources.

## Verified prerequisites and gaps

The inspected Configs revision is `45c4970edd02e95fb72af5e5ec22712979830efc`. It defines separate state files for infrastructure, guest configuration, and firmware. Its command wrapper calls these independent configuration roots workspaces.

The existing roots have these responsibilities:

| Root | State file | Relevant boundary |
| --- | --- | --- |
| Infrastructure | `opentofu.tfstate` | Includes Tack guests together with other guests, networking, and Cloudflare resources. |
| Guest configuration | `guest.tfstate` | Includes shared guest preparation and several Tack QA guests. |
| Firmware | `firmware.tfstate` | Manages hardware firmware configuration. |

The inspected Tack revision `9efb81e0f8e5cf22e8710be773af0344f0901f6d` registers only `tack_schema`. That resource manages migrations. Ansible still installs application images. Neither finding establishes a complete Tack application workspace.

The provider also executes guest commands through the Proxmox API. A token that permits that operation does not establish a read-only planning identity. Resolve the actual permissions before enabling live PR plans.

Use a dedicated planning login with a server-enforced fixed Go reader, as approved by the user. Return only approved deployment metadata, including image versions, declared nonsecret container settings, health, and applied migration version. Exclude passwords, application records, arbitrary commands, and file downloads. Enforce the fixed command and disable forwarding and interactive sessions in the server's SSH configuration. Keep the reader and its disclosure policy outside the planning account's write permissions. Supply planning jobs only this login's credential, never the Proxmox execution token or deployment credential. Providers must consume the restricted metadata response rather than a raw Docker API connection during planning. This reader inspects state; OpenTofu resources remain responsible for deployment.

## Establish the installed baseline

Direct SSH inspection on October 23, 2026, verified the application containers below. The commands listed container names and image references without reading environment values, application records, or credential files.

Installed application instances:

| Environment | Guest | Application image source | Audit consumer |
| --- | --- | --- | --- |
| QA | CT 217 | `d1ff0e791f626479a3299d3dcf16f9762cd27089` | Runs the matching source version. |
| QA | CT 223 | `d1ff0e791f626479a3299d3dcf16f9762cd27089` | Does not run on this guest. |
| Production | CT 117 | `460d25a99f5b4ab03f2e8c0ef44071737cc63f71` | Runs the matching source version. |
| Production | CT 123 | Docker lists neither running nor stopped containers. | Does not run on this guest. |

Production currently has one application instance. Container presence does not establish application health or load-balancer membership. Adoption must refresh exact digests, identities, mounts, and routing membership before changing ownership.

Neither installed source version supplies the required audited migration-status read. Production implements only migration apply; QA reads status outside audit recording. Do not infer the applied database version from an image tag or invoke apply to inspect it. Task 1 installs the fixed audited reader before schema import.

The GitHub environments `tack-qa-plan`, `tack-qa-apply`, `tack-prod-plan`, and `tack-prod-apply` now exist. Each permits deployments only from `main` and does not require a reviewer or waiting period. The implementation must install its separate restricted credentials and enable the verified workflows.

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

Extend image downloads with a configurable timeout, initially ten minutes, as approved by the user. Finish downloading and verifying all required images before stopping any application instance. A failed download stops the release while the current version continues running. Measure the separate 60-second outage limit from service interruption, excluding prior downloads. Emit a visible GitHub Actions warning when a download takes 30 seconds, including the image and elapsed time. Continue the download until completion or the ten-minute timeout.

The inspected `docker_compose` implementation checks whether project containers exist during `Read`, while its planned content changes use a file hash. It does not implement import or compare individual live service fields. The approved extension must correct these gaps through resource state and lifecycle behavior, rather than a command wrapper.

The current migration reader runs `docker compose run --pull never tack-ops migrate --output json`. It starts a temporary container and reads migrations from the installed image. A new release can contain migrations that image does not include. Extend `tack_schema` to consume candidate migration metadata bound to the release digest, compare it with the live database version during planning, and execute that same candidate during apply. Preserve its expected-version check and verify the resulting version. Do not install the candidate before PR planning merely to discover its migrations.

Publish the candidate migration list during the build, as approved by the user. Include each migration's version, name, and checksum, and bind the list to the exact release image digest and verified build. Planning compares that list with the live applied database version without executing candidate application code with production credentials. Before executing migrations, verify that the candidate image contains the migrations identified by the list. Reject a mismatch before changing the database.

The Docker container reader also omits environment and label refresh. Full drift reporting for those fields requires a general provider correction. Passing passwords through its ordinary `env` configuration would put them in OpenTofu resource state; use protected runtime secret references under the separate credential design.

Extend `ops deploy verify`, as approved by the user, to accept expected services and image digests from the deployment configuration. Check every declared instance and report missing containers, incorrect images, and failed health checks individually. Support different service placements without requiring both fixed container names on every guest. Keep QA functional tests as a separate required check.

For multiple application instances, stop routing new requests to the selected instance and allow up to 30 seconds for existing requests to finish, as approved by the user. Keep the other instances serving. Replace the selected container only after its existing requests finish. If requests remain at the deadline, cancel that update and restore normal routing without stopping the instance or terminating its requests. Implement this reversible preparation before process shutdown; extending the existing shutdown timeout alone does not satisfy it.

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

## Select unattended state credentials

The current application plan uses separate keys in the existing `tofu-state` bucket. Cloudflare's [permanent R2 credentials](https://developers.cloudflare.com/r2/api/tokens/) can restrict access to whole buckets. Exact-object and prefix restrictions require [temporary credentials](https://developers.cloudflare.com/r2/api/s3/temporary-credentials/), which expire and require a parent credential for renewal. Keep that broad parent credential outside GitHub runners.

The remaining choice changes Task 6's storage configuration:

1. Use a separate state bucket for QA and production with a permanent credential restricted to each bucket. Preserve the selected state filenames and encryption. This avoids a credential issuer.
2. Keep the shared bucket and install a fixed Go credential issuer on an existing protected host. The issuer authenticates each planning or applying identity and returns only its short-lived session. It exits after each request and does not require another daemon or container.

Separate buckets require four user-owned R2 tokens: one read-only planning token and one applying token per environment. The supplied mint-only capability supports this route. Complete bucket creation and other control-plane operations, then revoke their temporary provisioning token before creating the final four identities. The disposable proof verified that this sequence fits the current user-token quota without deleting unrelated credentials. Check total records, including expired tokens, before beginning final provisioning.

Recommend the separate buckets for the smaller implementation. The current plan does not select either option until the user answers this choice. Do not treat temporary test credentials as an unattended deployment credential system.

## Implement the approved design

Use the [Tack implementation plan](2026-10-22-tack-deployment-implementation.md) for exact interfaces, repository ownership, adoption steps, and acceptance tests. The [disposable test results](2026-10-22-tack-deployment-spikes.md) record the feasibility evidence and its limits.

This document preserves the approved choices and inspected source capabilities. The implementation plan supplies the development tasks; deployment readiness requires those tasks and their acceptance evidence.
