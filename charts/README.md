# Install the iSCSI CSI driver with Helm

The chart installs the node driver for static volumes by default. Set
`targetd.enabled` to `true` to install the controller for dynamic volumes.
Install one instance of the driver in a cluster.

The default driver image is the upstream canary. For targetd, set
`image.iscsi.repository` and `image.iscsi.tag` to an image built from this
revision. Set `imagePullSecrets` if the registry requires credentials. The
chart uses the same driver image on the controller and nodes. Each sidecar
image has its own repository, tag, and pull policy under `image`.
To pin an image by digest, include the digest in its repository value.

For the `targetd` trial on `jacobeatsspam/csi-driver-iscsi`, each branch push
runs the storage E2E tests. After they pass, the **Targetd test image** workflow
publishes Linux amd64 and arm64 images. Copy the full
`ghcr.io/jacobeatsspam/csi-driver-iscsi@sha256:...` reference from the workflow
summary into `image.iscsi.repository`. Install the chart from that commit.

For targetd, create a Secret in the release namespace with a `password` key.
Set `targetd.existingSecret` to its name. Do not put the password in Helm values.
The controller reads the password at startup. Restart it after changing the
Secret.

For example:

```yaml
image:
  iscsi:
    repository: ghcr.io/example/iscsi-driver
    tag: commit-sha
targetd:
  enabled: true
  backendID: storage-1
  endpoint: https://storage.example.org:18700/targetrpc
  pools: [vg-targetd]
  namespace: cluster-1
  targetIQN: iqn.2026-01.org.example:storage
  portals: ["192.0.2.20:3260"]
  existingSecret: csi-targetd-credentials
```

Keep the backend ID, ownership namespace, pool names, and target IQN stable for
existing volumes. For HTTPS with a private CA, create a ConfigMap in the release
namespace with a `ca.crt` key. Set `targetd.existingCAConfigMap` to its name.
Use numeric IPv4 addresses for the iSCSI portals.

To create a StorageClass with the chart, set `storageClass.create` to `true` and
set `storageClass.pool` to one of the configured targetd pools. Alternatively,
use [the example StorageClass](../examples/targetd-storageclass.yaml). The
example PVC uses a StorageClass named `targetd`. To use it with the chart's
StorageClass, set `storageClass.name` to `targetd`.

`kubeletDir` sets the host kubelet directory. `node.stateDir` sets the host
directory for connection records. Set it to the existing directory when you
migrate a driver installation. The default `/var/lib/iscsi.csi.k8s.io` retains
records across node restarts.

For a new targetd installation, save the values in `values.yaml` and run:

```sh
helm install csi-driver-iscsi charts/latest/csi-driver-iscsi --namespace kube-system -f values.yaml --wait
```

The targetd controller keeps an operation marker in
`csi-iscsi-targetd-barrier`. Helm retains that ConfigMap on uninstall. Inspect
the marker before reinstalling or recovering the controller. See the
[targetd guide](../docs/targetd.md) for recovery instructions.

Kubernetes does not allow changing `attachRequired` on an existing CSIDriver.
Follow the migration procedure in the targetd guide before changing modes on an
existing installation. Run only one controller for a backend and ownership
namespace.

After changing the chart, rebuild its archive and repository index:

```sh
helm package charts/latest/csi-driver-iscsi --destination charts/latest
helm repo index charts
```
