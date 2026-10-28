# iSCSI e2e tests

Run only on a fresh disposable cluster with no existing installation of
`iscsi.csi.k8s.io`. The harness refuses an existing CSIDriver, matching driver
DaemonSet, or driver ConfigMap before build or mutation. It does not support
adopting externally managed drivers: local-build verification is mandatory.
The inherited harness still changes host packages and has incomplete failure
cleanup; those are tracked separately in the PR #407 review backlog. No complete
mount/unmount live-cluster pass has been recorded yet.

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
the remote TCP check's nonzero status. Failure cleanup remains pending in the next task.

Each run generates a random `E2E_RUN_ID` and a new `iscsi-e2e-RUN_ID` namespace.
You can set `NAMESPACE` to a different **new** namespace and `E2E_RUN_ID` to a
1–20 character lowercase alphanumeric/hyphen identifier. Existing namespaces and
same-named PVs are rejected, including when their labels appear to match. A run
ID with existing namespace/PV/CSIDriver or driver-resource ownership labels is
also rejected, even when selecting a different namespace, so stale runs cannot
be adopted or cleaned up by a new invocation. Test
objects (including cluster-scoped PVs and installed driver objects) carry the
`iscsi.csi.k8s.io/e2e-run` ownership label; cleanup selects only that run. Creation
uses `kubectl create`, so a collision cannot silently overwrite resources.

The target has a run-specific name, selector and IQN. The PV uses the Service
ClusterIP rather than service DNS because host iscsiadm does not necessarily use
cluster DNS, and enables discovery for a fresh initiator. Driver registration
and node host paths remain global: run full harnesses sequentially on a cluster,
not concurrently. Separate clusters are required for parallel runs.

Standalone example fixture targets also require an explicit namespace:

```sh
kubectl create namespace my-disposable-target
make install-iscsi-server ISCSI_SERVER_NAMESPACE=my-disposable-target
make uninstall-iscsi-server ISCSI_SERVER_NAMESPACE=my-disposable-target
```

Those commands use the example's names within that namespace and refuse to
replace existing objects on installation. Uninstall selects only objects labeled
as this example, not the run-specific e2e driver or resources.
