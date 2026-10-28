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


create_shared_resources() {
    TEARDOWN_VOLUMES+=("$PV_B_NAME")
    local worker
    worker=$(sed 's/^/              /' "$SCRIPT_DIR/shared-io.sh") || return 1
    cat <<EOF | scenario_kubectl create -f -
apiVersion: v1
kind: PersistentVolume
metadata:
  name: $PV_B_NAME
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: iscsi.csi.k8s.io
    volumeHandle: $PV_B_NAME
    volumeAttributes:
      targetPortal: "$TARGET_PORTAL"
      iqn: "$IQN"
      lun: "2"
      portals: "[]"
      discovery: "true"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $PVC_B_NAME
  namespace: $NAMESPACE
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: $PV_B_NAME
---
apiVersion: v1
kind: Pod
metadata:
  name: $POD_B_NAME
  namespace: $NAMESPACE
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  nodeName: $SHARED_NODE
  containers:
    - name: test
      image: busybox:1.36
      command:
        - /bin/sh
        - -c
        - |
$worker
      env:
        - name: SHARED_IO_TIMEOUT
          value: "$E2E_SHARED_IO_TIMEOUT"
      readinessProbe:
        exec:
          command: ["sh", "-c", "test -s /tmp/io-progress"]
        initialDelaySeconds: 1
        periodSeconds: 1
      volumeMounts:
        - name: iscsi-vol
          mountPath: /mnt
  volumes:
    - name: iscsi-vol
      persistentVolumeClaim:
        claimName: $PVC_B_NAME
EOF
}

observe_shared_b() {
    local pod progress
    pod=$(scenario_kubectl get pod "$POD_B_NAME" -n "$NAMESPACE" -o json) || return 1
    python3 - "$pod" "$SHARED_B_UID" "$SHARED_NODE" <<'HEALTH' || return 1
import json,sys
p=json.loads(sys.argv[1]); uid,node=sys.argv[2:]
s=next((s for s in p.get('status',{}).get('containerStatuses',[]) if s.get('name')=='test'),{})
if (p.get('metadata',{}).get('uid') != uid or p.get('spec',{}).get('nodeName') != node
        or p.get('metadata',{}).get('deletionTimestamp') or s.get('restartCount') != 0
        or not s.get('ready') or 'running' not in s.get('state',{})):
    raise SystemExit('Shared target B worker interrupted or restarted')
HEALTH
    progress=$(scenario_kubectl exec "$POD_B_NAME" -n "$NAMESPACE" -- cat /tmp/io-progress) || return 1
    [[ "$progress" =~ ^[1-9][0-9]*$ ]] || { echo 'Shared target B has not completed checked I/O' >&2; return 1; }
    printf '%s\n' "$progress"
}

# Dynamic operation deadline is consumed by scenario_kubectl, which recomputes
# remaining time before every API operation, including both calls observing B.
shared_budget() {
    SCENARIO_OPERATION_DEADLINE=$SCENARIO_DEADLINE
    local limit
    for limit in "$@"; do
        if ((limit < SCENARIO_OPERATION_DEADLINE)); then SCENARIO_OPERATION_DEADLINE=$limit; fi
    done
    if ((SCENARIO_OPERATION_DEADLINE <= SECONDS)); then
        echo 'Shared target B progress or A removal deadline exhausted' >&2
        return 124
    fi
}

# The caller adds one explicit unverified-removal diagnostic for every failure,
# including a deadline expiring between the budget check and a kubectl call.
wait_shared_a_removal() {
    deadline=$((SECONDS + E2E_TEARDOWN_TIMEOUT))
    ((deadline <= SCENARIO_DEADLINE)) || deadline=$SCENARIO_DEADLINE
    while true; do
        shared_budget "$deadline" "$progress_deadline" || return 1
        count=$(observe_shared_b) || return 1
        ((count >= last_count)) || { echo 'Shared target B progress reset' >&2; return 1; }
        if ((count > last_count)); then
            last_count=$count
            progress_deadline=$((SECONDS + E2E_SHARED_IO_TIMEOUT))
        elif ((SECONDS >= progress_deadline)); then
            echo 'Shared target B checked I/O stopped advancing' >&2; return 1
        fi
        shared_budget "$deadline" "$progress_deadline" || return 1
        current=$(scenario_kubectl get pod "$POD_NAME" -n "$NAMESPACE" --ignore-not-found -o jsonpath='{.metadata.uid}') || return 1
        gone=0
        if [[ -z "$current" ]]; then gone=1
        elif [[ "$current" != "$previous" ]]; then echo 'A pod was unexpectedly replaced' >&2; return 1; fi
        result=0
        for validator in "${TEARDOWN_VALIDATORS[@]}"; do
            shared_budget "$deadline" "$progress_deadline" || return 1
            if scenario_kubectl exec "$validator" -n "$NAMESPACE" -- chroot /host /bin/sh -c \
                "$node_check" volume-clean check-lun "$IQN" "$PV_NAME" "${a_devices[$validator]}" / 1; then :
            else
                remaining=$?
                if [[ "$remaining" != 1 ]]; then return "$remaining"; fi
                result=1
            fi
        done
        if [[ "$result" == 0 && "$gone" == 1 ]] && ((count >= baseline + 2)); then break; fi
        if ((SECONDS >= deadline)); then
            return 1
        fi
        sleep 1 || return 1
    done
}

shared_target_scenario() {
    local a previous current pod count baseline validator remaining result
    local total=0 deadline last_count=0 progress_deadline gone validator_node observed=0
    local SCENARIO_OPERATION_DEADLINE=$SCENARIO_DEADLINE
    a=$(scenario_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o json) || return 1
    previous=$(python3 - "$a" <<'NODE'
import json,sys
p=json.loads(sys.argv[1]); node=p.get('spec',{}).get('nodeName'); uid=p.get('metadata',{}).get('uid')
if not node or not uid: raise SystemExit('Cannot identify shared target A node/pod')
print(node,uid)
NODE
    ) || return 1
    read -r SHARED_NODE previous <<< "$previous"
    create_shared_resources || return 1
    scenario_kubectl wait --for=condition=ready pod/"$POD_B_NAME" -n "$NAMESPACE" --timeout="${E2E_SCENARIO_TIMEOUT}s" || return 1
    progress_deadline=$((SECONDS + E2E_SHARED_IO_TIMEOUT))
    shared_budget "$progress_deadline" || return 1
    pod=$(scenario_kubectl get pod "$POD_B_NAME" -n "$NAMESPACE" -o json) || return 1
    SHARED_B_UID=$(python3 - "$pod" <<'UID'
import json,sys
uid=json.loads(sys.argv[1]).get('metadata',{}).get('uid')
if not uid: raise SystemExit('Cannot identify B worker')
print(uid)
UID
    ) || return 1
    # Wait for two completed fsynced iterations before disturbing A.
    while true; do
        shared_budget "$progress_deadline" || return 1
        count=$(observe_shared_b) || return 1
        ((count >= last_count)) || { echo 'Shared target B progress reset' >&2; return 1; }
        if ((count > last_count)); then
            last_count=$count
            progress_deadline=$((SECONDS + E2E_SHARED_IO_TIMEOUT))
        fi
        ((count >= 2)) && break
        sleep 1
    done
    baseline=$count
    shared_budget "$progress_deadline" || return 1
    snapshot_volume_devices scenario_kubectl || return 1
    local node_check
    local -A a_devices=()
    node_check=$(cat "$SCRIPT_DIR/node-volume-clean.sh") || return 1
    for validator in "${TEARDOWN_VALIDATORS[@]}"; do
        validator_node=$(scenario_kubectl get pod "$validator" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}') || return 1
        if [[ "$validator_node" == "$SHARED_NODE" ]]; then
            scenario_kubectl exec "$validator" -n "$NAMESPACE" -- chroot /host /bin/sh -c \
                "$node_check" volume-clean published "$IQN" "$PV_NAME" || return 1
            observed=$((observed + 1))
        fi
        a_devices[$validator]=$(scenario_kubectl exec "$validator" -n "$NAMESPACE" -- chroot /host /bin/sh -c \
            "$node_check" volume-clean snapshot-lun "$IQN" "$PV_NAME" '' / 1) || return 1
        if [[ "$validator_node" == "$SHARED_NODE" && -n "${a_devices[$validator]}" ]]; then total=$((total + 1)); fi
    done
    ((observed == 1)) || { echo 'Cannot observe A publication on its node' >&2; return 1; }
    ((total > 0)) || { echo 'No A LUN device observed; cannot prove shared-target removal' >&2; return 1; }
    scenario_kubectl delete pod "$POD_NAME" -n "$NAMESPACE" --wait=false --timeout="${E2E_SCENARIO_TIMEOUT}s" || return 1
    if wait_shared_a_removal; then :
    else
        result=$?
        echo 'Shared-target A removal was not verified; B survival cannot establish safe removal (S06/S07 gate)' >&2
        return "$result"
    fi
    SCENARIO_OPERATION_DEADLINE=$SCENARIO_DEADLINE
    echo 'Shared-target A LUN removed while checked B I/O continued'
    remaining=$((SCENARIO_DEADLINE - SECONDS)); ((remaining > 0)) || return 124
    E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining create_test_pod || return 1
    scenario_kubectl wait --for=condition=ready pod/"$POD_NAME" -n "$NAMESPACE" --timeout="${E2E_SCENARIO_TIMEOUT}s" || return 1
    current=$(scenario_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}') || return 1
    [[ -n "$current" && "$current" != "$previous" ]] || return 1
    remaining=$((SCENARIO_DEADLINE - SECONDS)); ((remaining > 0)) || return 124
    E2E_COMMAND_TIMEOUT=$remaining E2E_REQUEST_TIMEOUT=$remaining verify_payload || return 1
    observe_shared_b >/dev/null || return 1
    local payload
    payload=$(scenario_kubectl exec "$POD_B_NAME" -n "$NAMESPACE" -- cat /mnt/b-pattern) || return 1
    [[ "$payload" == 'iSCSI shared target B payload' ]] || { echo 'Shared target B data changed' >&2; return 1; }
}
