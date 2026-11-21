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
daemon="$cluster-iscsid"
node="$cluster-control-plane"
owner=$(docker inspect --format '{{ index .Config.Labels "iscsi.csi.k8s.io/e2e-kind-iscsid" }}' "$daemon")
pid=$(docker inspect --format '{{.State.Pid}}' "$node")
env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$daemon")
if [ "$owner" != "$cluster" ] || [ "$pid" = 0 ] || ! grep -Fxq "NODE_PID=$pid" <<<"$env"; then
  echo "The kind iSCSI daemon does not match the current node; refusing to start it." >&2
  exit 1
fi
case "${1:-}" in
  check) ;;
  start) docker start "$daemon" >/dev/null ;;
  *) echo "Expected check or start." >&2; exit 2 ;;
esac
