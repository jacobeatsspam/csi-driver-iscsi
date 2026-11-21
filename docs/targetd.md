# Dynamic volumes with targetd

The targetd controller provisions filesystem volumes for one targetd backend.
It supports `ReadWriteOnce` volumes with ext4 (the default) or XFS.
XFS volumes require at least 300 MiB. Snapshots, clones, expansion, raw block,
and iSCSI CHAP are not supported. The existing static installation remains
available without the controller.

The appliance must run a compatible targetd build and expose an LVM volume
group and iSCSI target. The Fedora 44 fixture passed with targetd 0.10.4-11
and permissive SELinux. Its enforcing policy blocked the rtslib lock file.

AlmaLinux 10.3 x86_64 passed the native targetd lifecycle test with verified
HTTPS and enforcing SELinux. The test used a local rebuild of the signed
Fedora 44 source RPM and a local policy for one rtslib lock file.
The stock policy blocks export creation. The node checks the device serial
against the targetd volume UUID before mounting. The tested AlmaLinux and
Fedora builds supplied matching serials. [AlmaLinux build, policy and test](targetd-almalinux.md)

Use a regular volume group with linear volumes; a thin pool is not required.
Adding physical volumes to the group makes their free space available for new
logical volumes. The controller does not initialize or extend that group.

Use an iSCSI target dedicated to this driver. targetd disables authentication
for the entire target when it creates an export. If the target already has
CHAP-protected exports, this also disables their CHAP enforcement. Restrict
access to the iSCSI network because the initial release does not use CHAP.

Reserve the configured ownership namespace exclusively for this controller.
Do not create, replace, rename, or delete its logical volumes outside the driver.
targetd deletes volumes by name, so a UUID check cannot protect against another
actor replacing a volume between the check and deletion. Keep backups.

Run exactly one controller for a backend and namespace. The deployment uses
one replica and `Recreate`, but this cannot fence a controller on a partitioned
or failed node. Before starting a replacement in that situation, ensure the old
controller cannot reach the appliance. The same applies when moving the
controller between clusters. The driver cannot fence a failed iSCSI writer:
before recovering a volume on another node, ensure the old node cannot write.

Give each node a unique initiator IQN. If a replacement node reuses a Kubernetes
node name, give it a new IQN. Reusing both identifiers lets an old detach
request affect the replacement node.

The pinned [provisioner v5.2.0](https://github.com/kubernetes-csi/external-provisioner/blob/v5.2.0/README.md#compatibility)
recommends Kubernetes 1.31; the pinned [attacher v4.8.0](https://github.com/kubernetes-csi/external-attacher/blob/v4.8.0/README.md#compatibility)
recommends Kubernetes 1.22. Confirm the sidecars and driver with your Kubernetes
version before upgrading. The manifests use the repository's canary image
placeholder: replace it in both the node and controller manifests with the same
image built from this revision.

## Configuration

Use HTTPS for the targetd management endpoint and restrict access to the
controller. The management credentials are distinct from iSCSI authentication.
The client verifies the server certificate; an additional CA can be mounted when
the appliance uses a private CA. The targetd build tested here rejects credentials
containing a colon. Only the controller driver container mounts the password.

Create a `config.json` with values appropriate for your appliance:

```json
{
  "backendID": "my-targetd",
  "endpoint": "https://targetd.example.org:18700/targetrpc",
  "pools": ["vg-targetd"],
  "namespace": "my-cluster",
  "targetIQN": "iqn.2026-01.example.org:storage",
  "portals": ["192.0.2.20:3260"],
  "username": "csi-controller",
  "passwordFile": "/etc/targetd/credentials/password",
  "caFile": "/etc/targetd/ca/ca.crt"
}
```

`backendID` and `namespace` must remain stable for existing volumes. `pools`
lists the pools that StorageClasses may select. `targetIQN` and `portals` must
describe the actual appliance target as seen by every node. Use a numeric IPv4
address for each iSCSI portal. Nodes match the address against the kernel's
device links. The HTTPS management endpoint can use a DNS name. Omit `caFile` when
the appliance certificate chains to a system CA. Each node needs a unique,
stable initiator IQN and working `iscsiadm`, `iscsid`, udev and iSCSI transport.

Create the ConfigMap and Secret in `kube-system` using files you manage securely:

```sh
kubectl -n kube-system create configmap csi-targetd-config --from-file=config.json
kubectl -n kube-system create secret generic csi-targetd-credentials --from-file=password=/secure/path/targetd-password
kubectl -n kube-system create configmap csi-targetd-ca --from-file=ca.crt=/secure/path/ca.crt
```

The CA ConfigMap is optional if `caFile` is omitted. The controller reads its
configuration and credentials at startup. After changing either, restart the
controller and wait for it to become ready. Preserve the stable backend ID,
ownership namespace, pool names and target identity for existing volumes.

## Installation and static migration

For a new installation, set the image in both manifests and create the
configuration and credentials above. Set the example StorageClass pool to your
volume group. From the repository root, run:

```sh
kubectl apply -f deploy/targetd/
kubectl -n kube-system rollout status deployment/csi-iscsi-controller
kubectl -n kube-system rollout status daemonset/csi-iscsi-node
kubectl apply -f examples/targetd-storageclass.yaml -f examples/targetd-pvc.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/targetd-example --timeout=3m
```

An existing static installation uses `attachRequired: false`. Kubernetes does
not allow changing that field on an existing CSIDriver object. Schedule a
maintenance window; do not assume an in-place patch or a live-mount migration
is safe. The following conservative migration requires stopping workloads that
use this driver. Test the procedure and rollback on your Kubernetes version
before using it with production data.

1. Record the current driver image and save the current CSIDriver and DaemonSet
   manifests. Stop all workloads using the driver and confirm their volumes are
   unmounted. Wait for any existing VolumeAttachments for this driver to clear.
   Check static PV `spec.csi.volumeHandle` values. The prefix `td1_` is reserved
   for dynamic volumes, so a static volume using it cannot use this migration.
2. Check each node for connector records under the old host directory, typically
   `/var/run/iscsi.csi.k8s.io`. Complete pending unpublishes before switching to
   `/var/lib/iscsi.csi.k8s.io`. The new node manifest stores records there so
   that they survive a reboot. Preserve unresolved records and investigate them
   before continuing.
3. Prepare the targetd configuration, Secret, optional CA and both updated manifests.
   Install the targetd controller and wait for it to be ready. Do not create
   dynamic claims yet.
4. Replace the existing CSIDriver object: delete `iscsi.csi.k8s.io`, then apply
   `deploy/targetd/csi-iscsi-driverinfo.yaml`. Apply the dynamic node manifest
   and wait for all node pods to become ready and register their initiators.
5. Resume a static workload first and verify read/write access. Then create a
   dynamic test claim and verify provisioning, mounting and deletion before
   resuming other workloads.

The controller accepts static volume handles without changing targetd. During
the replacement there may be pending attachment operations; keep workloads
stopped until the controller and node registrations are ready. If operations
remain stuck, inspect their events and VolumeAttachments before continuing.

After the controller is ready, replace the CSIDriver and update the nodes:

```sh
kubectl delete csidriver iscsi.csi.k8s.io
kubectl apply -f deploy/targetd/csi-iscsi-driverinfo.yaml
kubectl apply -f deploy/targetd/csi-iscsi-node.yaml
kubectl -n kube-system rollout status daemonset/csi-iscsi-node
```

The Deployment reports ready only when its controller can reach targetd and
find the configured pools, and the mutation barrier is clear. Check controller
logs and targetd connectivity if it does not become ready. Readiness does not
guarantee that iSCSI data paths work.

## Recover an uncertain targetd operation

targetd can keep an operation queued after the controller loses its response.
The controller writes a marker before each change to targetd. It clears the
marker only after targetd confirms success. If an operation fails or its outcome
is unknown, the marker remains in the `csi-iscsi-targetd-barrier` ConfigMap.
The controller stops lifecycle operations until an administrator resolves it.
The marker can remain after a failed marker update even if no targetd operation
was sent. Treat every marker as uncertain.

Do not delete or recreate the barrier ConfigMap to bypass this check. Do not
clear the marker just because the controller pod restarted. A previously queued
targetd operation can still run and change volumes or exports afterward.

1. Stop workloads that use the affected backend. Fence any node that can still
   write to a volume while its attachment is uncertain.
2. Scale `csi-iscsi-controller` to zero and wait for its pod to terminate. Make
   sure that no other controller can reach or change this backend.
3. Stop targetd on the appliance. Make sure that the targetd process and any
   storage operations it started have stopped. A timeout or a closed client
   connection is not proof that targetd canceled the operation.
4. Inspect the logical volumes, target mappings and iSCSI sessions. Reconcile
   the uncertain operation and make sure that no volume has multiple writers.
   Preserve data and follow the appliance's recovery procedures.
5. Restart targetd. Inspect its inventory and mappings again. After confirming
   the previous operation cannot resume, remove only the `pending` key from the
   barrier ConfigMap. Scale the controller to one and wait for readiness.

The marker names the operation, identifies the affected resources and includes
a random identifier. It contains no volume data or credentials. Removing the
marker requires an explicit edit:

```sh
kubectl -n kube-system edit configmap csi-iscsi-targetd-barrier
```

Delete the `pending` entry under `data`. Keep the ConfigMap itself. Retry or
clean up the affected claim only after you verify the appliance state.

## Rollback

Stop all workloads using this driver. While the controller is still running,
delete or otherwise safely retire dynamic claims and wait for their unmount,
VolumeAttachment removal and targetd export cleanup. A `Delete` reclaim policy
deletes the backing volume; use `Retain` and take backups when data must remain.
Do not roll back while dynamic volumes still need the controller: the static
driver cannot attach or delete them. Retained targetd volumes need a separate
recovery plan before removing the controller.

Once only static volumes remain, restore the saved static node DaemonSet and
wait for it to register. Delete the `iscsi.csi.k8s.io` CSIDriver object and
recreate it from `deploy/csi-iscsi-driverinfo.yaml` with `attachRequired: false`.
Remove the targetd controller only after its attachment operations have cleared.
Verify a static workload before resuming the rest. The targetd controller
resources, ConfigMaps and Secret can then be removed separately.

## Test the driver

From the repository root, run `make sanity-test` to exercise the CSI interface.
The controller tests use the production gRPC handlers, an in-memory targetd
backend, and a simulated node. The command also runs the static driver tests.

To exercise the full storage path, run `make local-act-e2e` on a Linux host
with KVM. It creates a disposable VM for targetd, LVM, kind, and the node driver.
See the [end-to-end test guide](../test/e2e/README.md).
