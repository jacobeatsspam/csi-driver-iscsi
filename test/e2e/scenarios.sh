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


create_test_pod() {
cat <<EOF | kubectl create -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  containers:
    - name: test
      image: busybox:1.36
      command: ["sh", "-c", "exec sleep 3600"]
      volumeMounts:
        - name: iscsi-vol
          mountPath: /mnt
  volumes:
    - name: iscsi-vol
      persistentVolumeClaim:
        claimName: ${PVC_NAME}
EOF
}

validate_scenarios() {
    E2E_SCENARIOS=${E2E_SCENARIOS-baseline}
    if [[ "$E2E_SCENARIOS" == all ]]; then
        E2E_SCENARIOS=republish,driver-restart,target-restart
    fi
    local scenario
    IFS=, read -r -a SELECTED_SCENARIOS <<< "$E2E_SCENARIOS"
    [[ -n "$E2E_SCENARIOS" && "$E2E_SCENARIOS" != *, ]] || { echo 'E2E_SCENARIOS must not be empty' >&2; return 2; }
    for scenario in "${SELECTED_SCENARIOS[@]}"; do
        case "$scenario" in baseline|republish|driver-restart|target-restart) ;;
            *) echo "Unsupported E2E_SCENARIOS entry: $scenario" >&2; return 2 ;;
        esac
    done
}

verify_payload() {
    local actual
    actual=$(kubectl exec "$POD_NAME" -n "$NAMESPACE" -- cat /mnt/test-file) || return
    if [[ "$actual" != "$EXPECTED_PAYLOAD" ]]; then
        echo 'E2E data verification failed: unexpected file contents' >&2
        return 1
    fi
}

# All scenario API calls are bounded by the remaining scenario budget in
# addition to the ordinary request/stream timeout configured by run-test.sh.
scenario_kubectl() {
    local remaining=$((SCENARIO_DEADLINE - SECONDS))
    if ((remaining <= 0)); then echo "Scenario $STAGE timed out" >&2; return 124; fi
    E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining kubectl "$@"
}

republish_scenario() {
    local previous current
    previous=$(scenario_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}') || return
    [[ -n "$previous" ]] || { echo 'Cannot identify publication pod' >&2; return 1; }
    scenario_kubectl delete pod "$POD_NAME" -n "$NAMESPACE" --wait=true --timeout="${E2E_SCENARIO_TIMEOUT}s" || return
    # create_test_pod never writes the payload; only the initial seed does.
    local remaining=$((SCENARIO_DEADLINE - SECONDS))
    ((remaining > 0)) || return 124
    E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining create_test_pod || return
    scenario_kubectl wait --for=condition=ready pod/"$POD_NAME" -n "$NAMESPACE" --timeout="${E2E_SCENARIO_TIMEOUT}s" || return
    current=$(scenario_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}') || return
    [[ -n "$current" && "$current" != "$previous" ]] || { echo 'Publication pod was not replaced' >&2; return 1; }
}

driver_restart_scenario() {
    local previous current name
    previous=$(scenario_kubectl get pods -n kube-system -l app=csi-iscsi-node -o json) || return
    local names
    names=$(python3 - "$previous" <<'DRIVER_NAMES'
import json,sys
pods=[p for p in json.loads(sys.argv[1]).get('items',[]) if not p.get('metadata',{}).get('deletionTimestamp')]
if not pods or any(not p.get('metadata',{}).get('uid') for p in pods):
    raise SystemExit('Cannot identify current driver pods')
for pod in pods: print(pod['metadata']['name'])
DRIVER_NAMES
    ) || return
    for name in $names; do
        scenario_kubectl delete pod "$name" -n kube-system --wait=true --timeout="${E2E_SCENARIO_TIMEOUT}s" || return
    done
    scenario_kubectl rollout status daemonset/csi-iscsi-node -n kube-system --timeout="${E2E_SCENARIO_TIMEOUT}s" || return
    current=$(scenario_kubectl get pods -n kube-system -l app=csi-iscsi-node -o json) || return
    python3 - "$previous" "$current" <<'DRIVER_REPLACED' || return
import json,sys
old={p['metadata']['uid'] for p in json.loads(sys.argv[1])['items'] if not p.get('metadata',{}).get('deletionTimestamp')}
pods=[p for p in json.loads(sys.argv[2]).get('items',[]) if not p.get('metadata',{}).get('deletionTimestamp')]
new={p.get('metadata',{}).get('uid') for p in pods}
if len(pods)!=len(old) or None in new or old & new:
    raise SystemExit('Driver pods were not replaced')
DRIVER_REPLACED
    local remaining=$((SCENARIO_DEADLINE - SECONDS))
    ((remaining > 0)) || return 124
    E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining verify_driver || return
}

target_restart_scenario() {
    local previous current identity name old_uid old_ip old_restarts result address
    previous=$(scenario_kubectl get pods -n "$NAMESPACE" -l "app=$TARGET_NAME" -o json) || return
    identity=$(python3 - "$previous" <<'TARGET_STATE'
import json,sys
pods=[p for p in json.loads(sys.argv[1]).get('items',[]) if not p.get('metadata',{}).get('deletionTimestamp')]
if len(pods)!=1: raise SystemExit('Expected one target container to restart')
p=pods[0]
s=next(s for s in p['status']['containerStatuses'] if s['name']=='tgtd')
print(p['metadata']['name'],p['metadata']['uid'],p['status']['podIP'],s['restartCount'])
TARGET_STATE
    ) || return
    read -r name old_uid old_ip old_restarts <<< "$identity"
    if scenario_kubectl exec "$name" -n "$NAMESPACE" -c tgtd -- sh -c 'kill -TERM 1'; then :
    else
        result=$?
        # The container may terminate the exec connection while handling TERM.
        case "$result" in 137|143) ;; *) return "$result" ;; esac
    fi
    while true; do
        current=$(scenario_kubectl get pod "$name" -n "$NAMESPACE" -o json) || return
        if python3 - "$current" "$old_uid" "$old_ip" "$old_restarts" <<'TARGET_RESTARTED'
import json,sys
p=json.loads(sys.argv[1]); uid,ip,count=sys.argv[2:]
if p.get('metadata',{}).get('uid')!=uid or p.get('status',{}).get('podIP')!=ip:
    raise SystemExit(2)
s=next((s for s in p.get('status',{}).get('containerStatuses',[]) if s['name']=='tgtd'),{})
ready=any(c.get('type')=='Ready' and c.get('status')=='True' for c in p.get('status',{}).get('conditions',[]))
raise SystemExit(0 if s.get('restartCount',0)>int(count) and s.get('ready') and ready else 1)
TARGET_RESTARTED
        then break
        else
            result=$?
            if [[ "$result" != 1 ]]; then echo 'Target pod changed; emptyDir persistence cannot be assumed' >&2; return "$result"; fi
        fi
        sleep 1
    done
    local remaining=$((SCENARIO_DEADLINE - SECONDS))
    ((remaining > 0)) || return 124
    address=$(E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining target_endpoint) || return
    [[ "$address" == "$old_ip" && "$address" == "$TARGET_HOST" ]] || { echo 'Target advertised address changed' >&2; return 1; }
}

run_scenarios() {
    echo 'Scenario baseline data check passed; overall teardown pending'
    local scenario
    for scenario in "${SELECTED_SCENARIOS[@]}"; do
        [[ "$scenario" != baseline ]] || continue
        STAGE=scenario-$scenario
        SCENARIO_DEADLINE=$((SECONDS + E2E_SCENARIO_TIMEOUT))
        case "$scenario" in
            republish) republish_scenario || return ;;
            driver-restart) driver_restart_scenario || return ;;
            target-restart) target_restart_scenario || return ;;
        esac
        local remaining=$((SCENARIO_DEADLINE - SECONDS))
        ((remaining > 0)) || return 124
        E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining verify_payload || return
        echo "Scenario $scenario data check passed; overall teardown pending"
    done
}
