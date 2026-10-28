#!/bin/bash

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

# Sourced by the harness; accepts explicit host/port for negative integration tests.
check_target_connectivity() {
    local host=$1 port=$2
    # The remote shell expands positional arguments, never interpolated shell text.
    # shellcheck disable=SC2016
    kubectl run "${CONNECTIVITY_POD_NAME:-iscsi-check}" -n "$NAMESPACE" --rm -i \
        --restart=Never --pod-running-timeout=120s --image=busybox:1.36 -- \
        sh -c 'if ! nc -z -w 3 "$1" "$2"; then echo "iSCSI target port is unreachable" >&2; exit 1; fi' \
        check "$host" "$port"
}
