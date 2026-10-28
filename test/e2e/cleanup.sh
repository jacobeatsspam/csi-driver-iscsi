#!/bin/bash
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# All commands must check their status: this function is also called from an
# EXIT trap and may run with errexit disabled or from a conditional.
cleanup_resources() {
    local result=0
    if [[ ${CLEANUP_CLUSTER:-0} == 1 ]]; then
        local selector="$OWNER_LABEL=$E2E_RUN_ID,$CLEANUP_LABEL=$CLEANUP_TOKEN"
        echo "Cleaning up invocation $CLEANUP_TOKEN in namespace $NAMESPACE..."
        # Graceful publication/device teardown is added by R06. Preserve the
        # current workload deletion semantics until that task implements it.
        kubectl delete pods -n "$NAMESPACE" -l "$selector" --grace-period=0 --force --ignore-not-found --timeout=60s || result=1
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
