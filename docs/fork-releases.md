# Fork releases

The fork publishes `ghcr.io/jacobeatsspam/csi-driver-iscsi` from
`temp-master` and `fork-release-*`. The initial integration includes
`complete-e2e-tests`; pinned assembly inputs are in `fork-integration.yaml`.
This branch also owns the fork-only workflow and these documents. Keep them out
of upstream PR branches.

## Branches

| Ref | Policy |
| --- | --- |
| `master` | Fast-forward mirror of upstream; no fork patches |
| Topic branches | One upstream PR each; branch independent work from upstream |
| `temp-master` | Combined candidate; may be rebuilt with force-with-lease |
| `fork-stable` | Last promoted source; never rewrite |
| `fork-release-YYYY.MM` | Optional maintenance branch from a deployed tag |
| `fork-vYYYY.MM.DD.N` | Immutable tag of a validated deployment |

Do not create stable or release tags until a candidate has passed storage tests.
Independent PRs must not contain integration merges. For dependent PRs, record
the parent in `depends_on` and merge parent before child.

## Assemble and update

1. Fetch `upstream` and `origin`. Update the manifest with the selected upstream
   SHA and exact topic SHAs. A refreshed remote ref alone is not an assembly lock.
2. In a clean worktree, create a temporary candidate branch at that upstream SHA.
   Merge each selected topic SHA with `--no-ff`, in dependency order. The initial
   branch already includes the pinned first topic.
3. Carry the three fork files listed in the manifest from the previous integration
   commit; commit their updated contents. Record integration conflict resolutions
   in separate commits with the affected PRs and rationale.
4. Check the tree and tests, then replace `temp-master` with the candidate.
   Push using `--force-with-lease` with the previously fetched remote SHA. Preserve
   deployed history through release tags and stable; never rewrite those refs.

When review requests changes, commit fixes on the original topic and repeat this
assembly. Keep fixes separate until propagated. When upstream merges a PR, advance
the pinned upstream SHA and omit that topic, including after squash merges. Recheck
dependent topics and conflict resolutions against the new upstream implementation.

## Publication

Pushing the integration or maintenance branch starts **Fork image**. A manual
dispatch is also allowed on those branches only. Unit tests run before publication;
Linux amd64 and arm64 images are assembled into a multiarch image tagged
`sha-<full-source-SHA>`. The workflow summary records the deployment digest. There
is no floating production tag and no automatic deployment. Tags are convenience
references; only the digest identifies immutable image contents. A rerun may produce
a different digest because the Dockerfile updates distribution packages.

GitHub Actions must be enabled on the fork with permission to write packages.
Publication uses its built-in `GITHUB_TOKEN`; no registry password is needed.
For unauthenticated pulls, set the GHCR package visibility to public after its
first publication. Otherwise configure the cluster's imagePullSecret for GHCR.
If the package already exists, grant this repository Actions access to it.

Inspect publication with:

```sh
gh run list -R jacobeatsspam/csi-driver-iscsi --workflow fork-image.yaml
gh run view -R jacobeatsspam/csi-driver-iscsi RUN_ID --log-failed
docker buildx imagetools inspect ghcr.io/jacobeatsspam/csi-driver-iscsi:sha-SOURCE_SHA
```

## Validate and promote

Unit tests are a publication gate, not storage acceptance. Run the native harness
against the exact candidate and inspect its evidence, including known failing
scenarios; never treat a published image as proof those scenarios passed. Validate
the published digest on a canary node with the deployment's targets: attach, filesystem
and block IO as applicable, pod/driver restarts, unpublish, and shared-target safety.
Then roll out the same digest to the remaining nodes.

In the deployment repository, override the `iscsi` container image from
`deploy/csi-iscsi-node.yaml` with
`ghcr.io/jacobeatsspam/csi-driver-iscsi@sha256:...`. Keep sidecar versions explicit.
Commit that deployment change.

For every promotion, retain a release record in the deployment repository with:
upstream and topic SHAs, source SHA, multiarch digest, validation evidence, deployment
revision, and previous working digest. Tag the source with
`fork-vYYYY.MM.DD.N`, push the tag, and advance `fork-stable` to that source
with a normal fast-forward push. Source tags do not rebuild images.

After an integration rebuild, the old stable tip may not be an ancestor. Make a
promotion commit with the candidate tree and both the previous stable tip and
candidate as parents; advance stable to that commit and tag it. Record both that
promotion SHA and the actual image source SHA. Do not rebuild or relabel the tested
image. This preserves stable history through integration rewrites.

Rollback by reverting the deployment digest to the previous validated digest.
Keep source tags and registry versions referenced by deployment records.

## Targeted backports

If integration includes unrelated changes, branch `fork-release-YYYY.MM` from
the deployed source tag. Cherry-pick only required fixes with `git cherry-pick -x`;
adapt dependencies explicitly and record any equivalent rewritten upstream commits.
Include the fork publication workflow if it is absent from the tagged source.
Push the maintenance branch to publish, validate its digest, and promote a new
release as above. Never move an existing release tag or overwrite a deployed image.
Ensure the original topic/integration also contains the fix, so the next full
upgrade cannot lose it.
