# Automatic application deployment through Configs pull requests

A reusable GitHub Actions workflow will publish OpenTofu plans on ready
pull requests and apply configuration after merge. Application release workflows
will create Configs pull requests and enable automatic merge after the required
checks pass.

This specification defines the agreed behavior. It does not activate deployment.
The implementation decisions below require further research before coding.

## Scope

The workflow must support applications through declared inputs rather than
application-specific code. Each consumer identifies its configuration repository,
release references, OpenTofu workspaces, and health checks.

Configs remains the source of deployment configuration. Application workflows
build releases and propose version changes there. They do not apply uncommitted
release overrides.

GitHub Actions runs the jobs. Reuse configsctl for OpenTofu execution, output
redaction, saved-plan verification, and infrastructure locks. Prefer existing
actions and libraries over a new deployment service or queue.

## Approaches

The selected approach creates a Configs PR for each application release.
A successful plan and the existing required checks permit automatic merge.
This approach records the deployed version in Git and gives the configuration
change its own checks.

Direct deployment from an application merge would require fewer GitHub
operations, but it would not record the version update in a Configs PR.
It is not the selected approach.

A workflow that requires a person to merge every release PR would preserve
the configuration review step but would not provide automatic deployment.
The selected approach uses the repository's merge requirements without adding
a separate confirmation for each release.

## Create release pull requests

1. An application merge starts its existing release build.
2. After the build succeeds, the reusable workflow creates a ready Configs PR
   containing the immutable release references and source commit.
3. The workflow enables automatic merge for that PR.
4. GitHub merges the PR only after the plan and all other required checks pass.

A repeated delivery of the same release must find the existing PR rather than
create a duplicate. Separate releases must retain their own identity and must
not silently replace an earlier queued release.

The bot must use authentication that permits the required PR checks and the
post-merge workflow to start automatically. GitHub documents restrictions on
events generated with the default workflow token.

## Publish plans on ready pull requests

Opening a ready PR, marking a draft ready, reopening a ready PR, or adding
a commit to a ready PR must start a plan for the affected workspaces.
Draft PRs do not run deployment plans.

Maintain one plan comment per PR. Update it with the commit, workspace results,
counts of additions, changes, deletions, and replacements, and expandable
details. Include the workflow run link. Never publish raw state, credentials,
or unredacted plan JSON.

A plan failure blocks automatic merge. An empty plan succeeds. OpenTofu's
detailed exit code for proposed changes also succeeds.

The check and comment must identify the revision that produced them. An older
run must not overwrite a newer result or satisfy the latest commit's required
plan check. Reusing a comment does not remove the individual workflow run records.

## Apply merged configuration

Each Configs merge starts an apply attempt for its affected workspaces.
Execute merges in order for each workspace and preserve each queued merge.
Independent workspaces may run concurrently when their infrastructure locks
permit it.

Each attempt checks out its exact merged configuration revision and creates
a fresh plan against the current infrastructure state. It applies the saved
plan from that attempt. It does not apply a PR preview from an earlier state.

Use the existing configsctl source checks and infrastructure locks. GitHub job
ordering does not replace locks that also protect manual operations.

For applications with QA and production, apply and validate QA before applying
production. A failed QA check prevents that release from deploying to production.

A failed apply or health check remains a failed deployment. The next queued
merge proceeds automatically and creates its own fresh plan. Failure does not
pause later changes or authorize clearing another operation's active lock.
The workflow does not automatically undo a failed apply.

## Implementation decisions to resolve

These gaps do not change the behavior above, but the implementation plan must
resolve them with source inspection and disposable tests.

| Area | Required result |
| --- | --- |
| Consumer inputs | Specify the release fields, configuration files, workspace mapping, and health-check interface without hardcoding one application. |
| Workspace selection | Include every root affected by shared modules, provider pins, or configuration inputs. Prove selection against the current Configs structure. |
| Runner access | Select credential delivery and verify state-backend, provider API, and lock-endpoint connections. Keep credentials private throughout cleanup and failure handling. |
| GitHub automation | Select bot authentication and prove that generated PRs run checks and that automatic merges trigger applies without manual workflow approval. |
| Apply ordering | Prove merge-order execution, duplicate-event handling, and continuation after failure. GitHub's built-in queue orders jobs by queue arrival, which alone does not guarantee merge order. |

The existing ordinary configsctl command supports unattended apply and creates
its own protected saved plan. The separate CI command currently rejects apply
without an acquired lock scope. Prefer the ordinary command if its credential
requirements fit the selected runner setup; do not bypass locking to reuse the
read-only CI command.

The builder may choose action versions, internal Go interfaces, and comment
formatting within these requirements. Deployment credentials, network access,
and the consumer contract must not remain assumptions at activation.

## Acceptance

1. Every new commit on a ready PR starts a plan. Converting a draft to ready also
   starts a plan, and a draft update does not start one.
2. A successful plan with proposed changes passes. A failed plan prevents
   automatic merge.
3. Repeated runs update one PR comment, and a late result cannot replace the
   latest revision's result.
4. A successful application release creates a Configs PR with the exact release
   references. A duplicate event does not create a second PR.
5. The generated PR merges automatically after the repository's requirements
   pass, and that merge starts an apply.
6. Applies use merged configuration and a fresh saved plan with existing
   configsctl locks. Concurrent manual operations retain their lock protection.
7. Several merged changes execute in order without dropping a release.
8. After an apply fails, the next queued merge executes against the resulting
   infrastructure state. Both attempts retain their actual results.
9. A QA failure prevents that release's production apply. A later release may
   proceed through its own QA checks.
10. A second consumer uses the same workflows through configuration inputs
    without application-specific orchestration code.
11. Cancellation and failure cleanup remove temporary credentials and plan
    files without interrupting unrelated operations.

## References

- [Configs CI](https://github.com/agoodkind/configs/blob/57aad4441835dcba8c4adf630e5312564fc75bff/.github/workflows/ci.yml) currently validates configuration without deployment plans or automatic applies.
- [configsctl apply implementation](https://github.com/agoodkind/configsctl/blob/12699cc1168257a28083cfcbd333a082fb58c4b1/cmd/configsctl/tofuexplicit.go) provides saved-plan verification and infrastructure locking.
- [GitHub workflow triggers](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow) explains bot-token event restrictions.
- [GitHub concurrency](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency) defines queue limits and ordering.
- [OpenTofu planning](https://opentofu.org/docs/cli/commands/plan/) defines previews, saved plans, and detailed exit codes.
