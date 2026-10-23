# Queued OpenTofu deployment design

Provide reusable GitHub Actions workflows that show deployment changes on every pull request update and deploy merged releases automatically. Each release completes QA, production, or verified recovery before another release starts.

Use the [implementation contracts](../plans/2026-10-22-deployment-contracts.md) for exact dependency revisions, permission boundaries, queue records, and recovery transitions.

## Define the project boundary

Keep the shared implementation in the public `agoodkind/tofu-workflows` repository. Accept caller configuration for projects, environments, OpenTofu roots, state backends, release inputs, checks, and credentials. Do not embed application names, host addresses, resource types, or database engines in the shared workflow.

Run automation on GitHub-hosted runners. Use existing public actions for supported operations. Use Go for additional ordering and result handling. Use Dockerfiles and Docker Compose for container builds and service definitions when a caller needs them. Do not add custom shell scripts or a continuously running deployment controller.

Use [dflook/tofu-plan](https://github.com/dflook/tofu-plan) and [dflook/tofu-apply](https://github.com/dflook/tofu-apply) for OpenTofu execution and PR reporting. Pin actions and container images to immutable revisions. Reusing an upstream action does not require copying its implementation into this repository.

Implement shared automation independently from application provider resources. A consumer must supply working declarative deployment resources before automatic apply can update that application. Do not hide missing resources behind command triggers or a second deployment system.

## Accept a consumer configuration

Each consumer supplies a deployment set with two ordered environments: validation and production. Consumers choose the display labels, such as QA. A deployment set identifies resources that must not change concurrently. Separate deployment sets may run independently only when their state and managed resources do not overlap.

Supply an independent configuration root, backend identity, and existing CLI workspace for each environment. A child module directory does not establish separate state. Do not infer isolation from directory names or use resource targeting to hide unrelated infrastructure in an application apply.

Require these consumer inputs:

| Input | Required behavior |
| --- | --- |
| Source repository and deployment branch | Identify the repository and branch that authorize release creation. |
| Initial commit | Identify the last release already installed before queue processing starts. Verify that baseline before enabling applies. |
| Configuration revision and root | Select the exact configuration revision and root for each environment. |
| Backend identity and CLI workspace | Identify the state that an apply can change and reject duplicate ownership. |
| OpenTofu version and provider lock file | Use identical tool and provider versions for a plan and its apply. |
| Environment settings | Select the state backend, variable files, credential environment, and health checks for each environment. |
| Release values | Identify exact application artifacts with immutable digests and typed nonsecret variables. |
| Required build and test checks | Identify the checks that must pass for the selected commit. |
| Recovery definition | Identify how to restore the previous working application release without reversing database history. |

Reject missing inputs with an error that identifies the required correction. Do not infer an environment, choose the newest image tag, or substitute a different commit when an artifact is unavailable.

Treat paths and references as data. Do not accept arbitrary shell text as a workflow input. Run consumer checks through pinned executable or container references with explicit arguments and a defined exit status.

## Plan every pull request revision

Start a plan for every commit pushed to an open PR. Plan every configured environment against its current managed state. Planning must not change deployed resources. Apply must use the backend lock.

Different PRs may run plans and tests independently. Shared QA must not become a reservation that blocks PR validation. Run upgrade and recovery tests on GitHub-hosted runners with real temporary dependencies and synthetic data.

Identify each result by PR, commit, environment, configuration revision, and run URL. Keep results for previous runs available in GitHub Actions. Update one current PR comment per environment. Prevent an older run from replacing the displayed result for a newer commit.

Show these items in each comment:

- State whether the plan succeeded or failed and whether it proposes resource changes.
- Show counts for creation, modification, replacement, and deletion.
- Show the commit and environment beside the result.
- Put detailed changes inside an expandable section.
- Link to the full run and its checks.

Remove secret values and protected runtime data from comments, summaries, diagnostics, and artifacts. Do not upload an unencrypted binary or JSON plan. Use ephemeral runner storage for plans that do not need to survive their job.

A PR plan is a preview. Deployments create a fresh plan after earlier releases finish. Do not apply a PR plan file against a later environment state.

## Process releases in merge order

Discover release commits from the deployment branch after the configured initial commit. Use first-parent history to establish the order in which the branch adopted changes. Identify a release by repository, deployment set, and exact commit. Repeated triggers for one release must not create another apply.

Use GitHub workflow concurrency to exclude simultaneous deployment sequences. Keep the complete QA and production sequence inside that exclusion. Disable cancellation of active deployments.

[GitHub concurrency](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#concurrency) supports multiple pending runs, but orders them by queue arrival rather than merge order. Native concurrency is the mutual-exclusion mechanism; commit history determines which release may deploy next.

The Go queue component must select the oldest unfinished release while it owns the deployment concurrency group. A trigger for a later commit may process that earlier release. Resolve its source, artifact digests, checks, and PR from the selected release rather than from the triggering event. If the earliest release is still building, do not skip it to deploy a newer release.

Use [GitHub deployment records](https://docs.github.com/en/rest/deployments/deployments) for verified release manifests, pre-mutation attempts, and final receipts. Store exact source/configuration identities, artifact digests, nonsecret release values, run references, and observed outcomes. Retain enough metadata to resume after a build artifact or historical status expires. A recovered release still has a failed deployment result.

Reconcile branch history with deployment records when a producer completes, a run is retried, or an operator requests reconciliation. After completing a release, explicitly dispatch the next ready release. Include a scheduled reconciliation to recover missing triggers. Do not depend on token-generated deployment events to start another workflow. A finite native queue must not silently discard a release from the logical queue.

After interruption, compare recorded attempts with actual managed state and checks before deciding whether to resume an apply or recover. Keep later releases waiting while the outcome remains unknown. Do not force-unlock OpenTofu state automatically.

## Apply and check each environment

Process one selected release in this order:

1. Verify the exact release artifacts and required checks.
2. Read the environment state and create a fresh OpenTofu plan.
3. Publish the current plan result and save the plan in the current job.
4. Apply that plan with the same configuration, variables, provider versions, and backend.
5. Check installed artifact identities and the consumer's required runtime behavior.
6. Record the observed result before starting the next environment.

Use [saved-plan apply](https://opentofu.org/docs/cli/commands/apply/#saved-plan-mode) to execute the plan from that attempt. The merge authorizes normal deployment of the release. Routine replanning does not require another human approval or a new PR review.

Configure dflook apply behavior for this authorization model. Its default comparison against the earlier PR plan must not block an otherwise authorized release merely because the preceding release changed the starting state. Preserve plan reporting and apply the exact plan generated for the current attempt.

For QA followed by production, a failed QA apply or required check prevents the production apply for that release. Start production deployment automatically as soon as QA passes all required functional tests and health checks. Do not add a waiting period after those checks pass. Create separate plans for the separate environment states; do not reuse QA's plan file in production.

## Recover from a failed deployment

Before changing an environment, identify its previous verified application release and the information required to restore it. Require upgrade tests to prove that the previous application remains compatible with database changes in the candidate release.

On apply or health-check failure, read actual managed state. Use the candidate configuration and restore only the declared application-release variables from the previous verified release. Validate a separate recovery plan against permitted and protected resource addresses. Never recover by applying the entire older configuration. Preserve committed database changes and audit history. Do not restore an entire database snapshot, reverse migrations, delete application data, or rewrite Git history as an automatic recovery action.

Check the recovered release before reporting recovery success. Record the original release as failed even when recovery succeeds. Continue to the next queued release only after the environment has a verified working state. If recovery fails or compatibility cannot be established, stop that deployment set and report the specific failure. Other independent deployment sets may continue.

A missing artifact or failing check before mutation does not require a recovery operation. Record the failure and the unchanged verified state explicitly.

## Separate permissions

Store generated deployment credentials in each consuming repository's protected GitHub environment secrets. Restrict apply credentials to workflows authorized from the deployment branch. Keep application and database passwords on the service hosts. Never copy them into the shared workflow repository.

Planning requires separate credentials that permit only the resource reads necessary to calculate a plan. Give PR reporting only the GitHub permissions needed to update the relevant comments and checks. Do not give PR test code deployment credentials or unrestricted production access.

Run PR builds and tests without production credentials. A protected workflow reads the resulting release manifest as data and uses an approved configuration revision and pinned providers for live planning. It must not execute PR scripts, PR workflow definitions, or provider binaries from the PR with live credentials. Validate artifact provenance, source commit, variable names and types, and image digests before planning. Reject a manifest that attempts to change the trusted configuration revision, check executable, or provider selection.

The consumer must establish how its provider authenticates before enabling live planning or applying. Validate the provider's real permissions; a credential label does not prove read-only access. Provider transport determines whether the consumer needs SSH, a service API identity, or another supported identity. Do not add an SSH deployment system when the provider already performs the managed operation through an API.

The later credential-lockdown project must prevent coding agents from reading service secret files or using unrestricted administrative access to bypass application permissions. This workflow does not itself establish that host protection.

## Verify the shared behavior

Use real GitHub workflow runs in a test consumer repository and a small real OpenTofu configuration. The fixture must produce an observable managed release change, a check failure, and a recoverable version change without production credentials or data. Persist synthetic resources through a real remote provider between ephemeral runners, and separately test the container-check adapter on a runner. Generic fixture success does not establish Tack runtime correctness.

Acceptance must establish:

| Case | Required result |
| --- | --- |
| Several PR commits | Every commit starts validation; the current comment displays the newest commit's result. |
| Parallel PRs | Independent validation does not reserve shared QA. |
| Three rapid merges | Each commit applies once, in merge order, without combining or skipping releases. |
| Builds finish out of order | A later build cannot cause its release to deploy first. |
| Missing or repeated trigger | Reconciliation discovers missing work; a repeated trigger does not repeat a completed apply. |
| State changes after a PR preview | The deployment reports and applies a fresh plan without another routine approval. |
| QA failure | Production does not change for that release. |
| QA success | Production starts after all required QA checks pass, without an added waiting period. |
| Production health failure | Recovery restores and verifies the previous compatible application version. |
| Failed recovery | Later releases for that deployment set remain stopped with the failure visible. |
| Runner interruption | A new run determines actual state before resuming or recovering. |
| Secret handling | Logs, PR comments, summaries, and downloadable artifacts do not disclose test secrets. |
| Untrusted PR execution | PR source cannot use apply credentials or execute unrestricted production operations. |

The builder may choose internal Go package boundaries, exact JSON types, retry timing, and supported timeout values. Document those choices in the implementation plan and verify the cases they control. Do not change the deployment order, approval policy, isolation requirements, or recovery boundary as an implementation convenience.
