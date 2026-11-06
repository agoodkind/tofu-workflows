# Validate deployment inputs

Use this Docker action to check a deployment configuration and optional release
metadata on a Linux amd64 GitHub Actions runner.

## Configure validation

1. Check out the repository that contains the reviewed deployment configuration.
2. Pin the action to a reviewed commit. Replace `REVIEWED_COMMIT_SHA` in the
   example with that commit's full SHA.
3. Set `config` to the deployment set JSON file relative to the checkout. The
   example uses a sample path; replace `deployment-set.json` with the actual path.

```yaml
steps:
  - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
    with:
      persist-credentials: false
  - uses: agoodkind/tofu-workflows@REVIEWED_COMMIT_SHA
    with:
      config: deployment-set.json
```

The action reports a failure when required inputs or configuration are invalid.

## Check release metadata

1. Set either `manifest` to a release JSON file or `artifact` to its ZIP archive.
   Use paths relative to the checkout.
2. Read the expected build run ID and attempt from authenticated GitHub metadata.
3. Pass those values as `expected-run-id` and `expected-attempt`.

## References

- [Deployment input and release contracts](https://github.com/agoodkind/tofu-workflows/blob/6c5e5e807881cbf7275ed54fe3562a318b9da77d/docs/superpowers/plans/2026-10-22-deployment-contracts.md#define-workspace-and-release-inputs)
- [GitHub Docker action support](https://docs.github.com/en/actions/reference/workflows-and-actions/dockerfile-support)
