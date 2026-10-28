# iSCSI e2e tests

Run only on a fresh disposable cluster with no existing installation of
`iscsi.csi.k8s.io`. The harness refuses an existing CSIDriver, matching driver
DaemonSet, or driver ConfigMap before build or mutation. It does not support
adopting externally managed drivers: local-build verification is mandatory.
The inherited harness still force-deletes workloads during cleanup; graceful
teardown is tracked separately in the PR #407 review backlog. No complete
mount/unmount live-cluster pass has been recorded yet.

The test builds the current checkout, including uncommitted changes, and installs
the local driver manifests. It prints the source revision, built image ID and
each verified driver pod imageID; it never installs an upstream canary instead.
Python 3, Go, make, Docker and kubectl are required.

Prepare disposable test nodes **before** running the harness: Linux, open-iscsi
(`iscsiadm`), an active `iscsid`, the kernel's iSCSI TCP transport, and a running
udev daemon are required. The harness only validates these prerequisites. Its
privileged validator mounts each node root read-only and runs the fixed checks
through `chroot /host` with the node PID namespace; it does not install packages,
load modules, enable services or start daemons. A failed check keeps the validator
unready and fails the test before the target or driver is installed. Zero eligible
nodes cannot produce a passing validation. On failure, the harness prints the
validator's readiness events (before any test credentials or application pods
exist), which identify the missing prerequisite.

Node preparation is a separate administrator operation, never automatic e2e
behavior. For a disposable Debian/Ubuntu VM, install `open-iscsi` using its package
manager, load `iscsi_tcp`, and start the node's `iscsid` and udev services using
that system's service manager. Other Linux distributions use their own package
and service names. These operations persist beyond test cleanup; use only a
disposable VM/node and verify the correct host before performing them.

For kind, the node container must have the initiator tools and active daemons,
and shares the host kernel modules. Use a node image/module loader compatible
with the host module compression. In our local environment the old kind image's
kmod lacked Zstandard support, and udev's container condition prevented startup;
those were resolved explicitly in the disposable node, outside the harness.
Removing a kind cluster removes its container/service changes, but does not undo
kernel modules loaded into the shared host kernel. A VM with a matching kernel
and module loader avoids that container-specific preparation.

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
the remote TCP check's nonzero status.

An EXIT trap cleans partially created resources on normal completion, errors,
INT (exit 130) and TERM (exit 143). Cleanup runs before a passing result is
printed; a cleanup error makes an otherwise successful test fail, while an
original test failure keeps its exit status. Local rendered manifests are
removed too. Missing resources are ignored so cleanup can be repeated.

Every invocation adds a fresh random `iscsi.csi.k8s.io/e2e-invocation` token in
addition to the run ID. Cleanup requires both labels. This prevents a failed
namespace-create race from deleting another invocation's resources even if the
caller reused a run ID. Preflight refusals never arm cluster cleanup.

For debugging only, set `E2E_RETAIN_ON_FAILURE=1` to retain failed-run resources
and rendered manifests; the harness prints the namespace, run ID, invocation
token and local manifest paths. Successful runs still clean up. Inspect and
remove retained resources using **both** ownership labels; do not delete an
unrelated namespace or driver installation. The default is cleanup on failure.

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

The target has a run-specific name, selector and IQN. The PV uses the single
ready target pod's validated IP and enables discovery for a fresh initiator.
The real baseline exposed that tgtd advertises its PodIP during discovery: using
the ServiceIP creates a node record for a different portal than login/device
lookup expects. PodIP also avoids depending on node-side cluster DNS. Driver registration
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

Static test PVs use `Retain`; deleting a Kubernetes object must not claim to
delete backend storage. Cleanup deletes owned workload pods normally and waits
for deletion. Before deletion, read-only validator commands snapshot block
device names for this run's IQN. After deletion, validators inspect the node's
`/proc/1/mountinfo`, run-specific iSCSI sessions/device paths, and captured block
devices. They must all disappear before PVC/PV, target, validator and driver
resources are removed. Unrelated IQNs and volumes are neither modified nor
required to disappear.

`E2E_TEARDOWN_TIMEOUT` defaults to 60 seconds and bounds the node teardown wait.
There is no forced deletion success path. If workload deletion or node
observation/teardown fails, cleanup fails and retains the target, driver,
claims/volume, namespace and available manifests for recovery. Do not delete a
retained target while a publication may still be mounted; resolve the reported
node state first. Early setup failures without a possible publication still
clean independent resources normally. A passing result is printed only after
graceful cleanup succeeds.

Known driver failure observed during the native baseline run: on Ubuntu 24.04
with k3s v1.32.10, the locally built driver delivered through the test registry
successfully mounted, formatted, wrote and read the scratch volume. Graceful
teardown then failed the verification gate: NodeUnpublishVolume logged
`the device is in use : 1` and returned success while the run's iSCSI session
and device remained. The verifier timed out, returned a failing result and
retained the fixture and supporting resources as intended. The cause remains
under investigation in static CSI task S06; duplicate mount aliases are a
hypothesis, not a confirmed diagnosis. This is a working regression detector,
not a passing end-to-end acceptance result. Fixing the driver is separate work;
do not weaken the cleanup assertions to make this baseline pass.

## Locally built target fixture

The harness builds `deploy/example/tgtd` from this checkout before creating
cluster resources, loads it into the selected kind cluster or pushes it to the
explicit test registry, and verifies the ready target pod's runtime image ID.
`E2E_TARGET_IMAGE` selects an explicit non-latest/non-canary tag. Registry delivery
requires it (for example `127.0.0.1:5000/iscsi-pr407-target:test`) and renders the
matching immutable pushed digest, never a different repository's digest. Kind
uses the loaded local image with `imagePullPolicy: Never`. The base image is
pinned; the reported built image digest identifies the actual fixture tested.

The target readiness probe queries live tgtadm configuration: the expected IQN,
online LUN1, and backing-file path must all match. Startup fails if the daemon
never becomes controllable, exits, or target configuration fails. The entrypoint
bounds each control call (`TGTD_CONTROL_TIMEOUT`, default 3 seconds, plus a
1-second forced-kill allowance) and terminates its own daemon on errors/signals
with TERM followed by KILL after `TGTD_SHUTDOWN_TIMEOUT` (default 3 seconds); it does not leave a marker that can
make a later failed daemon appear ready.

A new backing file is sparsely initialized to `DISK_SIZE` MiB. An existing regular
file of exactly that size is reused without truncation; mismatched sizes,
symlinks and non-files fail instead of overwriting data. The deployment mounts
`emptyDir` at `/var/lib/iscsi`, so bytes survive **container** restart in the same
pod. Deleting/replacing the target pod or deleting the test namespace discards
the scratch storage; it is not production persistence. Tests must not use real
data as a fixture. The existing S06 teardown gate remains failing until fixed.

For the standalone example, first build/load `iscsi-e2e-target:local` into your
disposable cluster; its default pull policy deliberately refuses a remote image
fallback. The main harness builds and selects its own image automatically.


## Publication and restart scenarios

`E2E_SCENARIOS` selects comma-separated cases: `baseline` (default), `republish`,
`driver-restart`, or `target-restart`. `all` runs the three restart/publication
cases in that order after the baseline write/read. Each case can also run alone,
for example `E2E_SCENARIOS=target-restart` with the usual disposable-cluster and
image settings. `E2E_SCENARIO_TIMEOUT` (default 180 seconds) bounds each case,
including API calls and data reads. Image verification shares one monotonic
budget across its pod query and all kind runtime inspection fallbacks; successful
slow inspections cannot reset that budget.

The workload pod only sleeps; the harness writes and syncs its payload once,
after the first mount. Recreated pods and restart verification only read it.
`republish` requires a newly created workload pod UID against the same PV/PVC.
`driver-restart` replaces the installed driver pods, waits for new UIDs and the
selected ready image, then reads the existing publication. `target-restart`
terminates the target container and requires its restart count to increase in
the **same** pod with the same PodIP before readiness/image and data checks.
Replacing the target pod is rejected because its emptyDir would be new storage.

A `Scenario ... data check passed; overall teardown pending` line reports only
that transition's data check. Overall success still requires the final graceful
node teardown gate. The observed S06 driver leak can therefore make the run fail
after all scenario data checks pass; it is not converted to a skip or success.
Failures report a scenario-specific stage without printing unexpected payloads.
