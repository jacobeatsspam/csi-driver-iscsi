#!/bin/bash

# Copyright 2021 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

work=$(mktemp -d)
driver_pid=
runner=()
if [[ -n ${GITHUB_ACTIONS:-} ]]; then
  runner=(sudo)
fi

cleanup() {
  if [[ -n $driver_pid ]]; then
    "${runner[@]}" kill "$driver_pid" 2>/dev/null || true
    wait "$driver_pid" 2>/dev/null || true
  fi
  "${runner[@]}" rm -rf --one-file-system -- "$work"
}
trap cleanup EXIT

if ! sanity_bin=$(command -v csi-sanity); then
  sanity_bin="$(go env GOPATH)/bin/csi-sanity"
  if [[ ! -x $sanity_bin ]]; then
    GOBIN="$work" go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@v5.6.0
    sanity_bin="$work/csi-sanity"
  fi
fi

CSI_SANITY_BIN="$sanity_bin" go test -mod=vendor -count=1 -v ./pkg/iscsi -run '^TestTargetdControllerSanity$'

endpoint="unix://$work/csi.sock"
"${runner[@]}" "bin/${ARCH:-amd64}/iscsiplugin" --endpoint "$endpoint" --nodeid "${1:-CSINode}" -v=5 &
driver_pid=$!

# shortcut: csi-test v5.6.0 panics on skipped group snapshots, remove after upgrading.
skipTests='Controller Server|GroupController VolumeGroupSnapshots|should work|should be idempotent|should remove target path'
"${runner[@]}" "$sanity_bin" --ginkgo.v \
  --csi.secrets="$PWD/test/sanity/secrets.yaml" --csi.testvolumeparameters="$PWD/test/sanity/params.yaml" \
  --csi.endpoint="$endpoint" --csi.mountdir="$work/mount" --csi.stagingdir="$work/stage" --ginkgo.skip="$skipTests"
