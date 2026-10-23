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
- Do not introduce a second deployment command outside managed provider resources.

## Verified prerequisites and gaps

The inspected Configs revision is `8f70434225fd2e26bf75c519134a7ab4320dcd24`. It defines separate state files for infrastructure, guest configuration, and firmware. Its command wrapper calls these independent configuration roots workspaces.

The existing roots have these responsibilities:

| Root | State file | Relevant boundary |
| --- | --- | --- |
| Infrastructure | `opentofu.tfstate` | Includes Tack guests together with other guests, networking, and Cloudflare resources. |
| Guest configuration | `guest.tfstate` | Includes shared guest preparation and several Tack QA guests. |
| Firmware | `firmware.tfstate` | Manages hardware firmware configuration. |

The inspected Tack revision `2b1d90ea65519e978ce8e425f3228c9c0542593d` registers only `tack_schema`. That resource manages migrations. Ansible still installs application images. Neither finding establishes a complete Tack application workspace.

The provider also executes guest commands through the Proxmox API. A token that permits that operation does not establish a read-only planning identity. Resolve the actual permissions before enabling live PR plans.

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
- [ ] Verify exact installed images and runtime audit records. Only then enable automatic production deployment after the audit work completes.

## Planning status

The shared workflow plan supplies its implementation contracts. This companion identifies the remaining Tack-specific work; it is not a completed implementation plan for the missing providers or their permission boundary. Do not report the complete Tack rollout as de-risked until those concrete plans and acceptance results exist.
