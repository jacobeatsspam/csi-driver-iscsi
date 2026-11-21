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
if docker inspect "$cluster-control-plane" >/dev/null 2>&1; then
  sessions=$(docker exec "$cluster-control-plane" ls -A /sys/class/iscsi_session)
  if [ -n "$sessions" ]; then
    echo "Active iSCSI sessions remain; refusing to delete the cluster." >&2
    exit 1
  fi
  mounts=$(docker exec "$cluster-control-plane" awk '/\/volumes\/kubernetes.io~csi\// {print}' /proc/1/mountinfo)
  if [ -n "$mounts" ]; then
    echo "Test volume mounts remain; refusing to delete the cluster." >&2
    exit 1
  fi
fi
if docker inspect "$cluster-iscsid" >/dev/null 2>&1; then
  owner=$(docker inspect --format '{{ index .Config.Labels "iscsi.csi.k8s.io/e2e-kind-iscsid" }}' "$cluster-iscsid")
  if [ "$owner" != "$cluster" ]; then
    echo "Refusing to remove an unowned iSCSI daemon container." >&2
    exit 1
  fi
  docker rm -f "$cluster-iscsid"
fi
kind delete cluster --name "$cluster"
