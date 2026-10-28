# Validation record and release gates

Recorded on 2026-10-27 for the PR #407 harness work. These are local results,
not a claim that the GitHub workflow has executed or that the driver is ready
for release. Driver implementations were unchanged.

## Go test migration (2026-10-28)

The test suite and its command doubles now use Go with Ginkgo v2 and Gomega.
All 150 specs passed after the integration rewrite; the count expands the
original Python subcases into individual specs. The Go result recorder and
Docker fixture runner retain the same status and cleanup checks.
`make e2e-lint`, the vendored driver-package checks, and the real Docker fixture
test passed. Driver code, root dependencies, and the vendor tree are unchanged.
The native iSCSI runs below were not repeated during this migration; their
existing teardown failures remain release gates.

## Commands

From a clean checkout with Python 3, Go, make, ShellCheck, yamllint and
[actionlint v1.7.12](https://github.com/rhysd/actionlint/releases/tag/v1.7.12):

```sh
make e2e-unit
make e2e-lint
go test -mod=vendor ./pkg/... ./cmd/...
make e2e-fixture-test
```

The last command needs a usable Docker daemon and exercises actual tgtd control
operations; it does not need Kubernetes, root host mounts or an initiator.
Override `ACTIONLINT=/path/to/actionlint` for the lint target if needed. The
unprivileged tests use isolated command doubles and temporary files; they do not
prove kernel storage integration.

For real native integration, use a fresh disposable Linux VM/node, prepare
open-iscsi/iscsid/udev and TCP transport separately, install native Kubernetes,
and configure a test registry accessible to both Docker and the node runtime.
For the local single-node VM, a registry at `127.0.0.1:5000` was mirrored over
HTTP in k3s containerd configuration. On a prepared cluster:

```sh
export KUBECONFIG=/path/to/private-disposable-kubeconfig
export E2E_DRIVER_DELIVERY=registry
export E2E_DRIVER_IMAGE=127.0.0.1:5000/iscsi-driver:test
export E2E_TARGET_IMAGE=127.0.0.1:5000/iscsi-target:test
E2E_SCENARIOS=all E2E_ARTIFACT_DIR=/tmp/new-restart-results make e2e-record
# After recovering/cleaning that invocation, use a different new result path:
E2E_SCENARIOS=shared-target E2E_ARTIFACT_DIR=/tmp/new-shared-results make e2e-record
```

The native workflow contains the corresponding preparation for a disposable
GitHub-hosted Ubuntu runner and pins k3s v1.32.10+k3s1. Normal harness execution
does not perform this host preparation. Do not run preparation on a shared host.

## Observed results

| Check | Local evidence |
| --- | --- |
| Unprivileged tests | Passing command/failure tests; run `make e2e-unit` for the current count. |
| Shell/YAML/workflow/license checks | Passing after fixing the fail-open boilerplate reference path and normalizing added-file headers. |
| Go packages | Compile/test command passed; driver packages under pkg/ and cmd/ currently contain no Go tests. |
| Real Docker fixture | Protocol readiness passed; known backing bytes survived restart; incompatible existing size was rejected; owned container/volume cleanup passed. |
| Registry image delivery | Local driver and target images deployed by matching immutable digest and verified ready runtime image IDs. |
| Native baseline data | Mount, ext4 format, write and read passed. |
| Native restart data | Republish, driver-pod restart and same-pod target-container restart all preserved the seeded payload. |
| Native final teardown | **Failed.** Driver returned success after unmount while the device/session remained. Harness failed and retained supporting resources. |
| Native shared-target case | **Failed A removal.** B had no restart and continued checked I/O (counter 522 to 540), but A's device remained. This is not a safe-removal pass. |
| Unsupported/unvalidated features | Explicit non-success gates, not skipped successes; see the [support matrix](README.md#supported-scenarios-and-feature-gates). |

The successful native data phases used Ubuntu 24.04.5, kernel
6.8.0-147-generic, k3s v1.32.10+k3s1 and containerd 2.1.5-k3s1.32 in an isolated
KVM VM. An earlier kind v1.27.3/containerd 1.7.1 environment passed image and
prerequisite checks, but its iscsid died during kernel-control communication at
login. That was an environment failure, not a passing storage integration run.

The native driver failure logged `the device is in use : 1` and left the scratch
iSCSI device/session after the publication mount disappeared. The cause is still
under investigation (static CSI work S06); duplicate mount aliases are a
hypothesis. It may mask the target-wide logout defect tracked separately as S07.
The harness does not repair the driver, force successful cleanup, or turn these
results green. Native CI should remain red until the actual driver behavior is
fixed and the gates pass.

Manual recovery during local validation first verified **both** publication
mounts were absent, then logged out only the generated scratch IQN, removed only
its stale connector records, and ran checked ownership-scoped cleanup. Manual
recovery is not part of a passing original test result. Retained fixtures must
not be removed while a publication could still be mounted.

## Evidence artifacts

`make e2e-record E2E_ARTIFACT_DIR=NEW_DIRECTORY` preserves the command's actual
status in `summary.json`, even after successful data-phase messages. It records
only exact phase markers, validated digest strings, exit status and selected
source/node version fields. HEAD is recorded as `source_revision`; `source_dirty`
is a boolean for any nonignored worktree changes, or null if unknown, with no raw
status paths. A dirty record is not an exact clean-commit snapshot. Raw output is
private temporary storage and discarded afterward; kubeconfig, Secret contents,
manifests, payloads and arbitrary log or
event messages are not uploaded. A secret-sentinel regression enforces this.
Timeout returns 124; feature gates return 78; malformed input returns 2.

The GitHub native job uploads that summary with `if: always()` and preserves the
failing command result. Unit and Docker-fixture jobs are required on pull
requests; native integration is manually dispatched to avoid pretending that
mocked tests or an unsuitable container kernel establish acceptance.
