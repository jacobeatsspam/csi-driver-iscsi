# iSCSI e2e tests

Run only on a disposable cluster. The inherited harness still changes host
packages and has incomplete resource cleanup; those are tracked separately in
the PR #407 review backlog. No live-cluster pass has been recorded yet.

The test builds the current checkout, including uncommitted changes, and installs
the local driver manifests. It prints the source revision, built image ID and
each verified driver pod imageID; it never installs an upstream canary instead.
Python 3, Go, make, Docker and kubectl are required.

For a disposable kind cluster:

```sh
E2E_KIND_CLUSTER=iscsi-test make e2e-test
```

The harness builds a unique image tag and loads it into that kind cluster. Set
kubectl's context to the same cluster before running. The driver uses pull policy
Never so an absent local image fails instead of falling back to a registry.

For other disposable clusters, provide a writable registry accessible to nodes:

```sh
E2E_DRIVER_DELIVERY=registry E2E_DRIVER_IMAGE=registry.example/test/iscsi:run-123 make e2e-test
```

The image is built, pushed, and deployed by digest. Node registry credentials
must already be configured. E2E_ARCH defaults to the local Go architecture;
select a compatible architecture explicitly for test nodes when necessary.

Run unprivileged installation contract tests without a cluster:

```sh
cd test/e2e && go test ./... -v
```

The fake-command tests prove build/delivery failures propagate, local manifests
select the built image and runtime digest mismatches fail. They do not establish
that a real node can mount an iSCSI LUN; that remains an integration gate.

The harness fails on an unreachable target, failed Kubernetes operations, readiness
expiration, or unexpected payload. `E2E_COMMAND_TIMEOUT` (default 330 seconds)
bounds each kubectl command, including streaming commands;
`E2E_REQUEST_TIMEOUT` (default 30 seconds) bounds API requests. Both require GNU
`timeout` and positive integer values. Failure diagnostics report the stage,
original exit status, pod status and event reasons. They intentionally omit pod
logs, event messages, manifests and unexpected payloads because these may expose
credentials. Diagnostics failing does not replace the original failure status.

For a separate negative connectivity integration check on a disposable cluster,
source `test/e2e/checks.sh`, set `NAMESPACE`, and call
`check_target_connectivity HOST PORT` under an outer timeout. The helper propagates
the remote TCP check's nonzero status. Resource isolation and failure cleanup
remain pending in the next tasks.
