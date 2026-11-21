# iSCSI end-to-end tests

## Requirements

- The runner needs Go, kind, Docker with buildx, and the iSCSI TCP kernel
  transport available on the host.
- The kind setup runs a test iSCSI daemon in the host network and makes `/sys`
  writable inside the node. Use a disposable host without other iSCSI sessions.

## Run the tests

Create a disposable kind cluster and run the main e2e tests:

```sh
make deploy-kind
make
make e2e-test
```

This command does not require `act` or QEMU. The kind setup uses the host iSCSI
kernel state, so use a disposable host with no other iSCSI sessions. For kind
delivery, the test temporarily masks the host systemd iSCSI service to prevent a second
daemon from interfering. It restores the previous mask afterward, requires
sudo and systemd, and refuses to run if the host daemon is already active.
If the test fails, leaves sessions, or is killed before it can restore the mask,
it retains the mask. After removing retained test workloads and confirming that
all iSCSI sessions are gone, check
`systemctl is-enabled iscsid.service`. If the test created the runtime mask,
remove it with `sudo systemctl unmask --runtime iscsid.service`.

To run the PR workflow locally in a new disposable VM:

```sh
make local-act-e2e
```

This requires Linux x86-64, KVM, QEMU, `qemu-img`, `xorriso`, `curl`, and
internet access. It downloads checksum-verified Ubuntu and act releases, copies
the current checkout including untracked, non-ignored files, and disposes of the
VM afterward. The VM uses 6 GiB of RAM, four CPUs, and a 24 GiB virtual disk.
Ignored files and Git history are not copied.

The Ubuntu image is cached at
`${TMPDIR:-/tmp}/csi-local-act-ubuntu-noble-20261017-<uid>.img` and verified
before each run. The system may clean it up; deleting it forces a new download.
Set `LOCAL_ACT_VM_IMAGE` to use a different copy of the same pinned image.
Interrupting the command stops the VM and removes its temporary files; the
cached image remains available for the next run.

The targetd suite runs Fedora 44 targetd 0.10.4-11 with a private TLS
certificate and a loop-backed LVM volume group. It writes through iSCSI,
checks the backing volume, and restarts targetd.

The Kubernetes tests mount static and dynamic volumes with attachment enabled.
The automated kind workflow uses one node. Its tests cover serial validation,
data after controller and node plugin restarts, two volumes on one target, and
the `Delete` and `Retain` reclaim policies.

To run this suite on a disposable Ubuntu VM, first run `make deploy-kind`
and `make iscsi`. Then run the targetd test.

```sh
E2E_DISPOSABLE_VM=1 make e2e-targetd-test
```

The fixture must have its own VM: it changes host-wide LVM and iSCSI state and
installs packages. It does not validate the NAS's SELinux policy.

- The cluster defaults to `kind`. Set `E2E_KIND_CLUSTER` consistently to use a different name.
- For an existing disposable cluster, use `make e2e-test` with its matching
  kubeconfig. Nodes need open-iscsi, iscsid and udev.
- For registry delivery, set `E2E_DRIVER_DELIVERY=registry` and set
  `E2E_DRIVER_IMAGE`, `E2E_TARGET_IMAGE`, and `E2E_TOOLS_IMAGE` to writable
  image tags in a registry reachable by the cluster, then run `make e2e-test`.

## Two-node targetd handoff

On November 21, 2026, a manual test used two Ubuntu 24.04 KVM guests in a
k3s v1.34.3+k3s1 cluster. Each guest ran its own kernel and iSCSI initiator.
Kubernetes provisioned a 384 MiB XFS claim. A pod on the first node wrote
512 KiB of random data and recorded its SHA-256 checksum.

The test paused the targetd process with the volume mounted. The management
API timed out and the CSI controller reported unready. The pod still read
the original data, wrote another file, and synced it. Controller readiness
returned after targetd resumed.

After the first pod terminated, Kubernetes removed its VolumeAttachment and
targetd reported zero exports. A pod on the second node mounted the same PV,
matched the checksum, read the file from the outage, and synced a new write.
Only the second initiator had an export. Deleting the claim removed the PV,
logical volume, and last export.

## Select scenarios

- `E2E_SCENARIOS` defaults to `all`. It runs each recovery scenario with a fresh fixture.
- Find scenario names in the `known` map in [config/config.go](config/config.go).
  Entries with a nonempty value are unavailable for the reason given there.
- Set `E2E_SCENARIOS` to the names you want, separated by commas without spaces.
- Run `btrfs-recovery` and `removal-recovery` alone.
- Use a kernel with Btrfs support for end-to-end testing.

### Removal recovery

- Run `make e2e-test E2E_SCENARIOS=removal-recovery` to test a failed state write after device removal.
- This separate scenario bind-mounts the saved record during device removal, so the final rename fails.
- It removes the fault and requires three CSI unpublish retries to succeed. The shared baseline volume must retain its data.

## Cleanup

- Run `make e2e-teardown` to delete the cluster when done.
- Teardown refuses to delete the cluster while any iSCSI session remains.
- Cleanup retains resources if node teardown cannot be verified. Do not remove
  retained resources while a volume may still be mounted. Set
  `E2E_RETAIN_ON_FAILURE=1` to retain resources after other failures.
