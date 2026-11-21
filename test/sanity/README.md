# CSI sanity tests

From the repository root, run both suites on a Linux test host:

```sh
make sanity-test
```

The command builds the driver. It uses `csi-sanity` from your PATH or Go
workspace. If neither contains the binary, it downloads and builds v5.6.0 in
a temporary directory. [Upstream CSI sanity tests](https://github.com/kubernetes-csi/csi-test/tree/v5.6.0/pkg/sanity)

The targetd suite calls the production controller over a temporary Unix socket.
It tests provisioning, attachment, deletion, retries, and CSI error codes.
It uses an in-memory targetd backend, a simulated Kubernetes registry, and a
simulated node. If your Go workspace contains `csi-sanity`, run the controller
suite alone with:

```sh
CSI_SANITY_BIN="$(go env GOPATH)/bin/csi-sanity" go test -mod=vendor -count=1 -v ./pkg/iscsi -run '^TestTargetdControllerSanity$'
```

The static suite starts the driver binary and tests its identity and node RPCs.
It skips operations that need a real iSCSI volume. The driver needs access to
`/var/run/iscsi.csi.k8s.io` on the test host. For real PVCs and storage I/O,
use the [end-to-end tests](../e2e/README.md).
