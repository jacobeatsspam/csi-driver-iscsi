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
