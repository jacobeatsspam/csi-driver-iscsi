# iSCSI end-to-end tests

This is a Go Ginkgo v2/Gomega suite for pre-existing iSCSI LUNs. The suite,
Kubernetes operations, node probes, data workers, target supervisor and result
recorder are Go programs. Kubernetes resources are built as typed objects and
managed through client-go. Image builds and delivery execute Go, Docker and
kind directly, without a shell harness or Python programs.

## Layout

- `e2e_suite_test.go`, `static_provisioning_test.go`: real cluster specs and
  suite lifecycle, enabled with the `e2e` build tag.
- `driver/`: static PV, PVC and workload pod builders.
- `testsuites/`: context-aware setup, scenarios, image verification hooks and
  ownership-scoped cleanup using Kubernetes clients.
- `images/`: checkout builds, kind/registry delivery and runtime image identity.
- `probe/`, `workload/`, `cmd/e2e-tools/`: read-only node observations and data
  operations in a purpose-built container.
- `targetfixture/`, `cmd/tgtd-fixture/`: bounded tgtd supervision and readiness.
- `internal/runner/`: Docker fixture checks and allowlisted result recording.

All dependencies use the root `go.mod`, `go.sum` and vendor tree, matching the
sibling CSI repos. No separate test module or installed Ginkgo CLI is required.

## Local checks

```sh
make e2e-unit
make e2e-lint
golangci-lint run
make e2e-fixture-test
```

Unit tests use Kubernetes fake clients, temporary files and Go subprocess
fixtures. They do not require a cluster and do not prove kernel integration.
Lint checks Go formatting/vet, manifest/workflow YAML and repository license
headers. Run the repository-wide `golangci-lint run` separately to match the
Static Checks workflow (validated with v2.15.0). The existing repository license
checker requires Python 3; it is separate from e2e execution. ShellCheck, yamllint and actionlint v1.7.12 are
needed for lint (`ACTIONLINT=/path/to/actionlint` can override its executable).

The Docker fixture check builds this checkout's target image, verifies actual
tgtd protocol readiness, writes known backing bytes, restarts the container,
checks those bytes, rejects an incompatible backing size and removes only its
owned container and volume. It needs Docker access, not an initiator or cluster.

## Cluster prerequisites

Use a fresh disposable cluster with no installation of `iscsi.csi.k8s.io`.
The suite refuses existing driver objects, reused namespaces and stale run
resources. Do not use a production NAS or an existing shared installation.

Prepare Linux nodes with open-iscsi (`iscsiadm`), a running iscsid, udev, and the
iSCSI TCP kernel transport before running. Node preparation is an administrator
operation; the suite does not install packages, load modules or start services.
Its validator mounts the node root read-only and observes host PID1 mountinfo,
processes, sysfs and device paths with a Go probe. Every node validator must be
ready and its image verified before any volume test can pass.

The local runner needs Go, make, Docker with the buildx plugin, and access to the disposable cluster.
kind is additionally needed for kind delivery. Select the node architecture
using `E2E_ARCH` when it differs from the runner (amd64 and arm64 are accepted).

## Run against kind

On already prepared disposable kind nodes:

```sh
export KUBECONFIG=/path/to/disposable-kind-kubeconfig
export E2E_DRIVER_DELIVERY=kind
export E2E_KIND_CLUSTER=iscsi-e2e
E2E_SCENARIOS=all make e2e-test
```

The kubeconfig context must be `kind-iscsi-e2e`. The suite builds the driver,
target and tools from this checkout and loads explicit run-specific tags into
that cluster. Local image tags must not be latest, canary, untagged or digests.
Every running driver, target and validator image is checked against its built
config identity, recorded by Buildx metadata or a hash-verified OCI export
when the exporter omits that field. Docker .Id is not assumed to be a config
digest. kind runtime inspection resolves the actual running image ID, never a
mutable desired tag as substitute evidence.

## Run with a registry

For a prepared native disposable cluster, use a registry accessible to both
Docker and every node runtime. Configure registry transport and credentials on
those nodes separately.

```sh
export KUBECONFIG=/path/to/disposable-kubeconfig
export E2E_DRIVER_DELIVERY=registry
export E2E_DRIVER_IMAGE=registry.example/iscsi-driver:e2e
export E2E_TARGET_IMAGE=registry.example/iscsi-target:e2e
export E2E_TOOLS_IMAGE=registry.example/iscsi-tools:e2e
E2E_SCENARIOS=all make e2e-test
```

All three images are built and pushed, selected by the matching repository's
immutable digest, and verified against ready runtime container identities.
OCI indexes and cached aliases are resolved through exact immutable platform
manifest descriptors to the checkout build config digest. A desired mutable
tag is never accepted as evidence of the running image.
The target uses persistent backing files in its pod's emptyDir, so container
restart preserves its LUNs. Replacing the target pod or changing its IP fails
the target-restart scenario. The actual ready target PodIP is the iSCSI portal;
the service ClusterIP is not used for discovery/login identity.

`make e2e-build` compiles `bin/iscsi-e2e.test`. `make e2e-test` executes that
binary directly. Set `GINKGO_FLAGS` for verbosity or reports. Empty focus/skip
selections fail, dry runs are rejected, and filtering away a requested scenario
cannot establish a successful requested test run.

## Scenarios and feature gates

Baseline always mounts LUN1, seeds once with exclusive creation, fsyncs and
verifies exact payload bytes. It is followed by the requested scenarios:

| Selection | Check |
| --- | --- |
| `baseline` | Mount, seed and read a static filesystem volume. |
| `republish` | Delete/recreate the workload pod and verify original bytes without reseeding. |
| `driver-restart` | Replace driver pods on the same nodes, verify image identities, then check original bytes. |
| `target-restart` | Restart the target container in the same pod/IP and check original bytes. |
| `shared-target` | Publish two LUNs on one node; verify A's mount, remove A while B continues checked I/O without restarting, verify A's device removal, then republish A without reseeding. |
| `all` | Republish, driver restart and target restart after baseline; shared-target is requested separately. |

`E2E_SCENARIOS` is a comma-separated list without whitespace. Requests containing
`chap-secret-ref`, `stats`, `raw-block`, `expansion` or `multipath` return status
78 with a reason before cluster access or image work. These features are
unsupported or unvalidated here. Mixed executable/gated requests are atomic;
unknown names, empty requests and malformed combinations return status2.

## Cleanup and failures

Each invocation has a random run ID and a separate ownership token. Resource
creation is create-only, and deletion rechecks both labels and object identity.
The suite snapshots owned node devices before removing workload pods, then
requires the publication mounts, captured devices, target paths and sessions
to disappear before deleting supporting target/driver objects, claims or the
namespace. Individual-LUN checks permit B's shared session and devices to remain.
Lost ownership, missing observations, residual devices and deadline expiry all
fail cleanup and retain supporting resources for investigation.

`E2E_RETAIN_ON_FAILURE=1` also retains the invocation after a scenario fails.
Do not remove retained target/driver resources while a publication might still
be mounted. Never treat manual recovery as a passing original test result.

Current native evidence includes a driver unpublish defect: the publication
mount disappears but its device/session remains. The suite must fail that gate.
No driver implementation is changed by these tests. See [validation](VALIDATION.md).

Timeout settings are positive integer seconds: `E2E_COMMAND_TIMEOUT` (330),
`E2E_REQUEST_TIMEOUT` (30), `E2E_SCENARIO_TIMEOUT` (180),
`E2E_TEARDOWN_TIMEOUT` (60), and `E2E_SHARED_IO_TIMEOUT` (10). Shared-target
removal consumes the narrowest remaining scenario, teardown and B-progress
budget. Cleanup has a separate bounded context so interruption cannot skip it.

## Recorded evidence and CI

```sh
E2E_SCENARIOS=all E2E_ARTIFACT_DIR=/tmp/new-iscsi-results make e2e-record
```

The artifact directory must be new. The Go recorder executes the compiled
Ginkgo suite, retains its actual status in `summary.json`, and enforces a whole
run limit (`E2E_RECORD_TIMEOUT`, default1800 seconds). Its private unlinked raw
output is discarded. Only exact successful data-phase markers, image digests,
exit status, revision/dirty state and allowlisted node version fields are
recorded; kubeconfig, annotations, payloads and arbitrary logs are excluded.
A successful data phase does not override a teardown failure. Suite-level
AfterSuite cleanup must succeed and emit verified-cleanup evidence; a native
child exit0 without that evidence is a failure too. Gates remain78,
input errors2, timeouts124 and interrupted runs128 plus the signal number.
Make itself returns nonzero for a failed recipe; the summary preserves the
suite's exact status. Background processes are cleaned up even if their leader
has already exited. A successful leader leaving background processes fails.

The PR workflow runs unit, lint and real Docker fixture checks. A manual native
job prepares a fresh hosted Ubuntu runner, starts pinned k3s and a test registry,
runs the Go suite and uploads only summary.json, including failed-run status.
It does not use continue-on-error or claim a skipped storage case passed.
