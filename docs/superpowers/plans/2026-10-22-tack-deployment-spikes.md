# Tack deployment feasibility results

Disposable tests exercised the proposed metadata, Compose, download, routing, migration, and state mechanisms. They used synthetic services and credentials. They did not deploy Tack or inspect production credentials or data.

## Results

The experiments established these results:

| Area | Experiment | Observed result | Implementation consequence |
| --- | --- | --- | --- |
| Restricted metadata | Real OpenSSH forced command and a Go reader against isolated Docker | The combined test passed in 1.097 seconds. It rejected shell, injection, file-transfer, forwarding, unsupported fields/targets, and policy writes. It excluded synthetic secrets from the response. | Use typed server-approved observations, pinned host keys, and administrator-owned policy. |
| Compose import and drift | OpenTofu with the built upstream provider and real containers | Import failed as unimplemented. A restart-policy change produced an unchanged plan. A typed observation prototype detected that change without recreating the container. | Extend import and live field comparison; retain Compose as the source. |
| Guest secret references | A secret file existed only on the Docker host | The provider mounted it read-only without downloading it to the runner. Compose emitted a misleading client-side file-existence warning. | Preserve absolute host paths and correct the warning rather than copying secrets. |
| Image downloads | Real Docker pulls through a buffered forwarding adapter with controlled response delay | A normal pull completed in 1.038 seconds. A 31.616-second pull warned; a 61.623-second pull completed under the longer timeout. An 80.525-millisecond shortened deadline canceled the request. The existing synthetic container kept running. | Propagate timeouts into guest execution and stream progress into a bounded completion result. |
| Actual guest execution | The unchanged pinned transport used a dedicated token on a disposable networkless Proxmox CT | A 65-second command completed in 67.336 seconds. The transport rejected output above one MiB, preserved nonzero status/stderr, and reported a one-second remote timeout after 1.792 seconds. The token could not execute in another CT. | Keep explicit remote timeouts and bounded helper output; client cancellation alone does not stop remote work. |
| Reversible routing | Real Traefik and two Go h2c services | The readiness-only experiment served 3,336 short requests without errors. A request exceeding thirty seconds completed after update cancellation and routing restoration. | Confirm proxy exclusion before starting the request-completion window. |
| Actual replacement | Sixteen clients issued POST operations while one container gracefully stopped and Compose recreated it | All 17,330 operations produced exactly one receipt. The test observed zero response errors, missing receipts, duplicate receipts, or backend protocol errors. Clients did not retry POSTs. | Separate reversible preparation from committed graceful shutdown. |
| Migration metadata | Exact Tack catalog function and embedded SQL | Deterministic output included all 23 migrations. Tests rejected edited SQL, incorrect source SHA, and incorrect synthetic image digest in 0.163 seconds. | Reuse `schemastatus.Available`; bind the final image digest externally after publication. |
| Separate state | Real OpenTofu with two local state roots | The states had distinct lineages. QA apply left production state bytes unchanged. Parent verification reran all three Compose/state checks successfully in 1.264 seconds. | Use independent application roots and state keys. |
| Encryption and locking | Real MinIO with OpenTofu S3 backends, generated synthetic credentials, and enforced encryption | Tests passed in 9.07 seconds. State/plans encrypted; missing/wrong keys failed; competing QA operations could not acquire the lock; production remained independent; cancellation released the lock normally. | Reuse the existing encryption identifiers and S3 lockfile configuration. |
| R2 authorization and locking | OpenTofu 1.13.2 on a GitHub-hosted runner with separate QA and production temporary sessions | The final test passed in 30.27 seconds. Encryption, conditional locks, cancellation release, and wrong-key rejection passed. Each identity could use its own exact state/lock objects and workspace prefix; cross-environment reads, writes, and listing returned HTTP 403. | Preserve the exact object and workspace-prefix grants in the application plan. |
| Hosted-runner connections | Real Cloudflare Access TCP clients connected to the QA PVE API and guest SSH | The normal Go client verified the original PVE TLS identity. OpenSSH verified the original guest's trusted host key. Wrong or missing Access identities could not connect. | Keep socket addresses separate from TLS and SSH identities; use fixed TCP origins rather than browser-facing HTTP proxying. |
| Existing host-lock compatibility | A fixed Go command and the unchanged configsctl lock script used the same temporary filesystem records | Tests passed in 1.852 seconds, including 4200-second acquisition/renewal, preserved expiry under a legacy renewal, contention, expiry, and rejected unsafe input. | Preserve the existing one-hour migration allowance with a seventy-minute supervisor lease; the legacy 900-second cap applies only to extend. |
| Migration client cancellation | Real Docker Compose ran a synthetic twenty-second process in a disposable container | Killing the Compose client left the container running after three seconds. Sending TERM left both client and container running. | Enforce a deadline inside the migration container and inspect its execution state before another mutation. |
| Direct bucket credentials | OpenTofu 1.13.2 used separate bucket-scoped credentials without session tokens | The hosted test passed in 29.19 seconds. Apply identities supported encrypted state and locks but could not access sibling buckets. A read-only identity could initialize and preview with `-lock=false` but could not write state, delete it, or create locks. | Separate buckets can support unattended deployment without an additional credential issuer. The storage choice remains open. |

## Routing measurements

The replacement test used Traefik v3.0.4, the existing ten-second health interval, and a three-second timeout. Frontend clients reused HTTP/1.1 connections; backend responses used HTTP/2 over cleartext TCP, called h2c.

| Measurement | Result |
| --- | --- |
| Concurrent POST clients | 16 |
| Successful POST operations | 17,330 |
| Failed, missing, duplicate, or unexpected operations | 0 |
| Unrenewed preparation to restored proxy routing | 22.309 seconds |
| Proxy exclusion before replacement | 9.851 seconds |
| Graceful shutdown with an intentionally late request | 3.241 seconds |
| Proxy restoration after recreation | 6.409 seconds |

The test admitted a three-second request after observing zero active requests. Graceful shutdown waited for that request before process exit. Compose recreated the container only after exit, while the other instance continued serving. Preparation lease expiry restored readiness; commitment disabled that automatic restoration.

An initial test-router priority did not select the intended instance deterministically. The corrected replacement run used an explicit higher priority and verified the selected-instance response. The final replacement figures above come from that corrected run.

## Tested versions and limits

The source inspection used Tack `9efb81e0f8e5cf22e8710be773af0344f0901f6d`, guest provider `a1b623c7b6f9969033e5a91408a69cbb26917e62`, and Docker provider `f5b5f8c4aa7dd333d36f6cc5a4327d316a7d2e73`. Docker was 29.9.2. The local Compose/state tests used OpenTofu 1.12.6 and Compose 5.6.1; the built provider embeds Compose 2.35.1. The real R2 test used the shared workflow's OpenTofu 1.13.2 pin.

The download adapter tested Docker pull behavior separately from the real Proxmox execution test. The latter used the unchanged guest provider on CT 100 with a dedicated `VM.Guest.Exec` identity. Canceling polling after 500 milliseconds left the remote command running; a later observation confirmed completion within its ten-second remote limit. The synthetic sentinel retained its process ID throughout. The proxy still needs its planned remote-timeout correction and bounded progress helper.

The reviewed fixed Go helper acquired and renewed the existing hypervisor lock during this guest test. Cleanup removed the CT, disk, role, user, token, temporary executable, and local credential file before releasing the matching lock. Independent queries then confirmed the missing CT configuration, empty disk list, and missing role and user.

The original migration provider allows 3600 seconds. A fifteen-minute lease cannot exclude another owner for that duration after runner loss. A second local interoperability test verified a 4200-second lease without changing the existing file format. The Docker cancellation experiment separately established that terminating the Compose client does not terminate its container. The implementation plan therefore requires an absolute deadline inside the migration process, retained execution metadata, and confirmed completion before another migration.

The [final R2 run](https://github.com/agoodkind/tofu-workflows/actions/runs/39912510056) verified exact object grants together with separate workspace-list prefixes. Cleanup deleted the disposable bucket and all six temporary GitHub secrets. Cloudflare accepted revocation of both temporary API tokens, and subsequent verification rejected them. The existing application state bucket did not change. Temporary-session renewal for unattended deployment requires a separate credential-lifetime decision; this test does not establish that mechanism.

The [Cloudflare connection run](https://github.com/agoodkind/tofu-workflows/actions/runs/39912775603) verified both permitted connections and denied identities. Direct HTTP proxying had returned a browser challenge; the final test used TCP forwarding with the default Go User-Agent and normal origin verification. Cleanup stopped the connector and deleted the test tunnel, DNS records, Access applications, service identities, GitHub secrets, and child API token. The source credential remains active and unchanged.

The [separate-bucket run](https://github.com/agoodkind/tofu-workflows/actions/runs/39915679148) used direct R2 credentials rather than temporary sessions. Its child tokens expired after two hours for test cleanup; the S3 authentication path supports lasting bucket-scoped credentials. It verified separate apply access and read-only previews without placing the provisioning credential in GitHub.

The metadata proof exercised its fixed reader and synthetic Docker targets. The completed resource must test the full supported field schema and Tack audit integration. The routing proof used small Go protocol applications, not Tack's real handlers or databases. It therefore does not replace full application acceptance.

Cleanup removed all disposable containers, networks, and test volumes. The private test sources and detailed results remain available for review; production repositories received no implementation changes from these experiments.

## Plan coverage

The [implementation plan](2026-10-22-tack-deployment-implementation.md) assigns the results to concrete work:

| Requested item | Implementation task |
| --- | --- |
| Define protected metadata and test secret exclusion | Task 1 |
| Define Compose fields, live comparison, and adoption | Task 3 |
| Test and specify warning/timeout behavior | Task 2 |
| Define routing and request completion | Task 5 |
| Define independent state and adoption | Task 6 |
| Complete interfaces, dependencies, and acceptance | Tasks 1 through 7, with the shared workflow contracts |

These experiments support the selected architecture and identify specific implementation corrections. They do not establish deployment readiness before that implementation and its acceptance tests exist.
