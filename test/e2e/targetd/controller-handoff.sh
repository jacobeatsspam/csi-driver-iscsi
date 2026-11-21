#!/usr/bin/env bash

# Copyright 2026 The Kubernetes Authors.
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

cluster=${E2E_KIND_CLUSTER:-kind}
if [[ ${E2E_DISPOSABLE_VM:-} != 1 || $# != 1 || ! -d $1 ]] || ! systemd-detect-virt --vm --quiet || [[ $(kubectl config current-context) != "kind-$cluster" ]]; then
  echo 'Controller handoff requires the disposable kind fixture and its work directory.' >&2
  exit 2
fi
driver=iscsi.csi.k8s.io
for ((attempt=0; attempt<60; attempt++)); do
  first_id=$(kubectl get csinode "$cluster-control-plane" --ignore-not-found -o "jsonpath={.spec.drivers[?(@.name=='$driver')].nodeID}")
  [[ $first_id == v1:* ]] && break
  sleep 1
done
[[ $first_id == v1:* ]] || { echo 'The kind initiator did not register.' >&2; exit 1; }
peer="targetd-handoff-$$"
iqn="iqn.2026-01.test.csi:$peer"
encode() { printf '%s' "$1" | base64 -w0 | tr '+/' '-_' | tr -d '='; }
second_id="v1:$(encode "$peer"):$(encode "$iqn")"

CGO_ENABLED=0 go build -mod=vendor -o "$1/controller-handoff" ./test/e2e/cmd/controller-handoff
kubectl create -f - <<YAML
apiVersion: storage.k8s.io/v1
kind: CSINode
metadata:
  name: $peer
spec:
  drivers:
    - name: $driver
      nodeID: $second_id
YAML
trap 'kubectl delete csinode "$peer" --wait=true --timeout=30s' EXIT
# shellcheck disable=SC2016 # The controller container expands this script.
kubectl -n kube-system exec -i deployment/csi-iscsi-controller -c iscsi -- sh -ec '
  file=$(mktemp)
  trap "rm -f -- \"$file\"" EXIT
  cat > "$file"
  chmod 0700 "$file"
  "$file" "$1" "$2"
' sh "$first_id" "$second_id" < "$1/controller-handoff"
