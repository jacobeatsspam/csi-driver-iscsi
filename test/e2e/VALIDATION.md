# Validation and acceptance gates

## Native-Go rewrite: 2026-10-28

The implementation now uses actual Ginkgo v2 cluster specs, typed Kubernetes
clients, Go node/data tools and Go target supervision. The shell harness and its
command-order tests were replaced with tests of the Go behavior. Driver source
under pkg/ and cmd/ was not changed.

Current checks:

| Check | Result |
| --- | --- |
| Go helper tests | 235 Ginkgo/Gomega specs pass across 10 packages; no cluster access required. |
| Root module | go mod tidy -diff produces no changes; vendored driver packages compile. |
| Formatting/vet/license/YAML/workflow checks | Pass, including modern Go build-tag license handling. |
| Repository-wide golangci-lint v2.15.0 | Zero issues with default settings and with the e2e build tag. |
| Go target in real Docker | Pass: protocol-ready LUN, backing bytes survive restart, incompatible size rejected, owned resources removed. |
| Native Go restart suite | Baseline, republish, driver restart and target-container restart data passed; child and recorder both exited1 on the residual-device teardown gate. |
| Native Go shared target | B retained its UID, zero restarts, and checked-I/O progress15 to45 while A was absent; A device removal failed and both child/recorder exited1. |
| Final image pipeline | Passed against real Docker29/containerd storage and the VM registry, including missing config metadata, hash-verified local OCI export, cached-index aliases and raw registry digest verification. |

The helper tests cover filesystem observations and API/process failure paths;
they are not substitutes for a real mount, restart or teardown test. The `e2e`
build tag keeps cluster specs out of ordinary unit runs. Unsupported feature
requests return78; malformed requests and dry runs return2. An empty Ginkgo
selection cannot report a successful acceptance run.

## Native Go evidence

The disposable Ubuntu24.04.5/kernel6.8.0-147/k3s1.32.10 VM used a private test
registry and fresh generated run identities. The Go read-only validators,
Go-supervised target, direct TCP connectivity and all three runtime image checks
passed. The target-container restart retained the same pod/IP and increased its
restart counter. Native data phases passed without reseeding after restarts.

The current driver still returned a successful unpublish after logging
`the device is in use : 1`, leaving generated devices/sessions. The corrected
suite's global AfterSuite failed and TestE2E returned1. The recorder preserved
command_exit_code1, exit_code1 and overall failed for both primary runs. Shared
results contain only baseline as a successful data phase; B continuing I/O is
not proof of successful A removal.

Validation initially found a false-pass bug: Ginkgo2.21 attached an ordered
DeferCleanup failure to the last skipped spec and returned0. That attempt was
invalidated. Cleanup now runs in global AfterSuite, and the recorder also rejects
a native exit0 without explicit verified-cleanup evidence. Two regressions
exercise that acceptance contract. Primary native runs were repeated after the
fix and correctly failed the driver gate.

Repeated real image builds also exposed Docker29 OCI-index aliases and an
exporter that omits the config digest from Buildx metadata. The implementation
now obtains the exact build config from metadata or a hash-verified export of
its immutable local descriptor, and resolves immutable registry/runtime
platform descriptors to that config. It hashes raw descriptor bytes rather than
trusting a mutable tag or Docker .Id semantics. Unit regressions and a final
real image-only pipeline passed after the raw-hash hardening; scenario and
probe code were unchanged by that final image-only check. A resourceVersion conflict seen
during safe recovery also received bounded fresh-read retries with six
regressions; UID and both ownership-label guards remain required on every
attempt.

After each failed invocation, recovery first verified both publication mounts
were absent, then removed only the generated target IQN and matching connector
records. Manual recovery does not change the failed original result. Private
summaries are retained under /tmp/iscsi-native-go-vm/results-all-fixed and
results-shared; the earlier invalidated attempt is separately labeled. These
working-tree runs explicitly record source_dirty=true. Scratch mounts, sessions,
connectors and owned resources were verified absent before shutting down the VM;
its disks, SSH key and kubeconfig were removed. No GitHub workflow
execution is claimed.

## Reproduction

```sh
make e2e-unit
make e2e-lint
golangci-lint run
go test -mod=vendor ./pkg/... ./cmd/...
make e2e-fixture-test
```

For native tests, prepare a fresh disposable Linux Kubernetes node with
open-iscsi/iscsid, udev and TCP transport. Provide a registry reachable from
both Docker and the node runtime; configure its transport separately.

```sh
export KUBECONFIG=/path/to/private-disposable-kubeconfig
export E2E_DRIVER_DELIVERY=registry
export E2E_DRIVER_IMAGE=registry.example/iscsi-driver:test
export E2E_TARGET_IMAGE=registry.example/iscsi-target:test
export E2E_TOOLS_IMAGE=registry.example/iscsi-tools:test
E2E_SCENARIOS=all E2E_ARTIFACT_DIR=/tmp/new-restart-results make e2e-record
# After safely recovering that invocation if necessary, use a new results path:
E2E_SCENARIOS=shared-target E2E_ARTIFACT_DIR=/tmp/new-shared-results make e2e-record
```

The recorder executes the compiled Go test binary directly, preserves its actual
status in summary.json, and discards private raw output. Reports expose only
successful data-phase markers, validated image digests, source revision/dirty
state and selected node version fields. Credentials, node annotations, arbitrary
logs and payload contents are not uploaded. A dirty record identifies the
checkout revision and explicitly says the tested working tree differs from it.
Native CI uploads only summary.json with if:always; failures remain failures.

## Behavior preserved during conversion

| Behavior | Regression coverage |
| --- | --- |
| Requests validated before cluster/build work; mixed gates atomic | config: unit cases plus the actual compiled suite binary with unavailable tools/kubeconfig. |
| Current-checkout builds, explicit tags, immutable registry selection | images: build/delivery failures, repository matching, invalid and ambiguous identities. |
| Actual running image identity and aggregate deadline | images: ready/status checks, kind runtime resolution, desired-tag false-positive regression. |
| No adopted or overwritten resources | testsuites: existing namespace/driver/run refusal, create failures and ownership races. |
| Read-only host prerequisites | probe: temporary host-root/process/transport fixtures, executable symlinks and byte snapshots. |
| PID1 mounts, owned sessions/paths/captured devices | probe: residual versus unobservable state, unrelated IQNs and per-LUN semantics. |
| Seed once, fsync and exact-byte persistence | workload: exclusive creation, mismatch and write/read failure cases. |
| B progress means completed checked I/O | workload: bounded Go subprocesses, failed initialization, missing progress parent and no false advancement. |
| Publication and restart identities | testsuites: pod UID, node coverage, target pod/IP/container restart and no reseeding. |
| Shared A removal while B continues | testsuites: known A mount/device, B identity/counter progress and narrow API/teardown deadlines. |
| Safe final teardown and retention | testsuites: residual/unobservable nodes, ownership loss, changed validators, remaining cluster finalizers, and deadline-bound resourceVersion conflict retries that preserve UID/ownership. |
| Target readiness and backing protection | targetfixture: exact IQN/LUN/path/online parsing, two LUNs, byte preservation, refusal and bounded shutdown. |
| Recorder status/security/process cleanup | internal/runner: failures after data success, gates, signals, timeouts, descendants and allowlisted evidence. |

Each component received an independent subagent review. Findings were fixed with
regressions, including shared-target deletion deadlines, ownership loss during
cleanup, missing /tmp in the scratch image, unsafe unused PID1 signaling, and
incorrect desired-tag evidence during kind identity resolution.

## Historical native results: 2026-10-27

These results came from the earlier shell implementation and are retained as
context, not as proof that the rewritten Go suite passed.

In a disposable Ubuntu24.04.5/kernel6.8.0-147/k3s1.32.10 VM, baseline filesystem
mount/write/read and republish, driver-pod restart and same-pod target-container
restart preserved the seeded bytes. Final teardown failed: NodeUnpublishVolume
returned success after unmount while the generated target's device/session
remained (log message `the device is in use : 1`). The shared-target case also
failed A removal while B continued checked I/O without restarting.

This is the S06 cleanup gate; it can mask the separate S07 target-wide logout
concern. Tests must not force the results green or repair driver behavior.
Manual recovery, when needed, first verifies both publication mounts are absent,
then removes only the generated scratch IQN/connector records and owned resources.
It does not change the failed original result. An earlier container-only kind
attempt failed iscsid/kernel communication and did not establish native storage
acceptance. No GitHub-hosted native workflow execution is claimed here.
