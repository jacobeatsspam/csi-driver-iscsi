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

# Observe through validators while the driver and target are still available.
# Snapshot device identities before deleting the publication, then wait for
# unpublish to remove mounts, devices and the run-specific target session.
verify_volume_teardown() {
    local deadline=$((SECONDS + E2E_TEARDOWN_TIMEOUT)) remaining result validator volume
    local node_check
    node_check=$(cat "$SCRIPT_DIR/node-volume-clean.sh") || return 1
    local -a volumes=("$PV_NAME")
    if declare -p TEARDOWN_VOLUMES &>/dev/null; then volumes=("${TEARDOWN_VOLUMES[@]}"); fi
    for validator in "${TEARDOWN_VALIDATORS[@]}"; do
      for volume in "${volumes[@]}"; do
        while true; do
            remaining=$((deadline - SECONDS))
            if ((remaining <= 0)); then
                echo 'Timed out waiting for run volume teardown; supporting resources retained' >&2
                return 1
            fi
            if E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining kubectl exec "$validator" -n "$NAMESPACE" -- \
                chroot /host /bin/sh -c "$node_check" volume-clean check "$IQN" "$volume" "${TEARDOWN_DEVICES[$validator]}"; then
                break
            else
                result=$?
                if [[ $result != 1 ]]; then
                    echo 'Cannot verify node volume teardown; supporting resources retained' >&2
                    return "$result"
                fi
            fi
            sleep 1
        done
      done
    done
}

snapshot_volume_devices() {
    local validators node_check validator validator_status query=${1:-kubectl}
    node_check=$(cat "$SCRIPT_DIR/node-volume-clean.sh") || return 1
    validators=$("$query" get pods -n "$NAMESPACE" -l "app=$INIT_NAME" -o json) || return 1
    validator_status=$("$query" get daemonset "$INIT_NAME" -n "$NAMESPACE" -o json) || return 1
    validators=$(python3 - "$validators" "$validator_status" <<'VALIDATORS'
import json, sys
pods = json.loads(sys.argv[1]).get('items', [])
desired = json.loads(sys.argv[2]).get('status', {}).get('desiredNumberScheduled', 0)
nodes = [pod.get('spec', {}).get('nodeName') for pod in pods]
ready = all(any(condition.get('type') == 'Ready' and condition.get('status') == 'True'
                for condition in pod.get('status', {}).get('conditions', []))
            and not pod.get('metadata', {}).get('deletionTimestamp') for pod in pods)
if desired < 1 or len(pods) != desired or len(set(nodes)) != desired or not all(nodes) or not ready:
    raise SystemExit('Cannot verify teardown without every node validator ready')
for pod in pods:
    print(pod['metadata']['name'])
VALIDATORS
    ) || return 1
    mapfile -t TEARDOWN_VALIDATORS <<< "$validators"
    for validator in "${TEARDOWN_VALIDATORS[@]}"; do
        TEARDOWN_DEVICES[$validator]=$("$query" exec "$validator" -n "$NAMESPACE" -- chroot /host /bin/sh -c \
            "$node_check" volume-clean snapshot "$IQN" "$PV_NAME") || return 1
    done
}

# All commands must check their status: this function is also called from an
# EXIT trap and may run with errexit disabled or from a conditional.
cleanup_resources() {
    local result=0
    if [[ ${CLEANUP_CLUSTER:-0} == 1 ]]; then
        local selector="$OWNER_LABEL=$E2E_RUN_ID,$CLEANUP_LABEL=$CLEANUP_TOKEN"
        echo "Cleaning up invocation $CLEANUP_TOKEN in namespace $NAMESPACE..."
        if [[ ${VOLUME_MAY_BE_PUBLISHED:-0} == 1 ]]; then
            if ! snapshot_volume_devices kubectl; then
                echo 'Cannot snapshot node devices; retaining resources for safe recovery' >&2
                return 1
            fi
        fi
        if kubectl delete pods -n "$NAMESPACE" -l "$selector" --wait=true --ignore-not-found --timeout="${E2E_TEARDOWN_TIMEOUT:-60}s"; then
            if [[ ${VOLUME_MAY_BE_PUBLISHED:-0} == 1 ]] && ! verify_volume_teardown; then
                echo 'Retaining target, driver, claims and namespace after failed teardown' >&2
                return 1
            fi
        else
            result=1
            if [[ ${VOLUME_MAY_BE_PUBLISHED:-0} == 1 ]]; then
                echo 'Workload deletion failed; retaining target, driver, claims and namespace' >&2
                return 1
            fi
        fi
        kubectl delete pvc -n "$NAMESPACE" -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete pv -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete deployment,service -n "$NAMESPACE" -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete daemonset -n "$NAMESPACE" -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete daemonset,configmap -n kube-system -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete csidriver -l "$selector" --ignore-not-found --timeout=60s || result=1
        kubectl delete namespace -l "$selector" --ignore-not-found --timeout=60s || result=1
    fi
    [[ -z ${TARGET_MANIFEST:-} ]] || rm -f -- "$TARGET_MANIFEST" || result=1
    [[ -z ${DRIVER_MANIFEST_DIR:-} ]] || rm -rf -- "$DRIVER_MANIFEST_DIR" || result=1
    return "$result"
}

finish_run() {
    local result=$1 cleanup_result=0
    trap - EXIT ERR INT TERM
    if [[ "$result" != 0 && "$E2E_RETAIN_ON_FAILURE" == 1 ]]; then
        echo "Retaining failed invocation $CLEANUP_TOKEN: namespace=$NAMESPACE run=$E2E_RUN_ID" >&2
        echo "Local manifests: ${TARGET_MANIFEST:-none} ${DRIVER_MANIFEST_DIR:-none}" >&2
    else
        cleanup_resources || cleanup_result=$?
        if [[ "$cleanup_result" != 0 ]]; then
            echo "E2E cleanup failed (original exit $result)" >&2
            if [[ "$result" == 0 ]]; then result=$cleanup_result; fi
        fi
    fi
    if [[ "$result" == 0 ]]; then
        echo 'E2E test PASSED: iSCSI volume mounted and writable; cleanup completed'
    fi
    exit "$result"
}
