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

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Names are stable within a run and unique across runs. A namespace supplied by
# the caller must also be new: never adopt or delete an existing namespace.
E2E_RUN_ID=${E2E_RUN_ID:-$(python3 -c 'import secrets; print(secrets.token_hex(6))')}
if [[ ! "$E2E_RUN_ID" =~ ^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$ ]]; then
    echo 'E2E_RUN_ID must be 1-20 lowercase alphanumeric/hyphen characters' >&2; exit 2
fi
NAMESPACE=${NAMESPACE:-iscsi-e2e-$E2E_RUN_ID}
if [[ ! "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
    echo 'NAMESPACE must be a DNS label (at most 63 characters)' >&2; exit 2
fi
TARGET_NAME=iscsi-target-$E2E_RUN_ID
PV_NAME=iscsi-pv-$E2E_RUN_ID
PVC_NAME=iscsi-pvc-$E2E_RUN_ID
POD_NAME=iscsi-pod-$E2E_RUN_ID
INIT_NAME=iscsi-init-$E2E_RUN_ID
CONNECTIVITY_POD_NAME=iscsi-check-$E2E_RUN_ID
OWNER_LABEL=iscsi.csi.k8s.io/e2e-run
CLEANUP_LABEL=iscsi.csi.k8s.io/e2e-invocation
# Never reuse an invocation token, even when the caller reuses a run ID. This
# makes cleanup safe after failed/ambiguous creates and concurrent name races.
CLEANUP_TOKEN=$(python3 -c 'import secrets; print(secrets.token_hex(12))')
CLEANUP_CLUSTER=0
TARGET_MANIFEST=
DRIVER_MANIFEST_DIR=
VOLUME_MAY_BE_PUBLISHED=0
declare -a TEARDOWN_VALIDATORS=()
declare -A TEARDOWN_DEVICES=()
E2E_RETAIN_ON_FAILURE=${E2E_RETAIN_ON_FAILURE:-0}
if [[ "$E2E_RETAIN_ON_FAILURE" != 0 && "$E2E_RETAIN_ON_FAILURE" != 1 ]]; then
    echo 'E2E_RETAIN_ON_FAILURE must be 0 or 1' >&2; exit 2
fi
IQN=iqn.2026-01.com.test:storage-$E2E_RUN_ID
LUN=1

# Bound API calls as well as streaming exec/run commands. kubectl's request
# timeout alone does not bound a remote process attached through exec.
E2E_COMMAND_TIMEOUT=${E2E_COMMAND_TIMEOUT:-330}
E2E_REQUEST_TIMEOUT=${E2E_REQUEST_TIMEOUT:-30}
E2E_TEARDOWN_TIMEOUT=${E2E_TEARDOWN_TIMEOUT:-60}
for duration in "$E2E_COMMAND_TIMEOUT" "$E2E_REQUEST_TIMEOUT" "$E2E_TEARDOWN_TIMEOUT"; do
    if [[ ! "$duration" =~ ^[1-9][0-9]*$ ]]; then
        echo 'E2E timeouts must be positive integer seconds' >&2
        exit 2
    fi
done
command -v timeout >/dev/null || { echo 'GNU timeout is required' >&2; exit 2; }
kubectl() {
    timeout --foreground "${E2E_COMMAND_TIMEOUT}s" kubectl --request-timeout="${E2E_REQUEST_TIMEOUT}s" "$@"
}

STAGE=preparation
failure_diagnostics() {
    local result=$1
    trap - ERR
    echo "E2E failed during $STAGE (exit $result)" >&2
    # Status and event reasons only: pod logs, event messages, manifests and
    # remote command output may contain CHAP credentials or other secrets.
    kubectl get pods -n "$NAMESPACE" -o 'custom-columns=NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason' >&2 || true
    kubectl get events -n "$NAMESPACE" -o 'custom-columns=TYPE:.type,REASON:.reason' >&2 || true
    kubectl get pods -n kube-system -l app=csi-iscsi-node -o 'custom-columns=NAME:.metadata.name,PHASE:.status.phase,REASON:.status.reason' >&2 || true
    exit "$result"
}
trap 'failure_diagnostics "$?"' ERR
# shellcheck source=test/e2e/cleanup.sh
source "$SCRIPT_DIR/cleanup.sh"
trap 'finish_run "$?"' EXIT
trap 'failure_diagnostics 130' INT
trap 'failure_diagnostics 143' TERM

echo "=== iSCSI CSI Driver E2E Test ==="

# Build and deliver the checkout before mutating Kubernetes resources.
# shellcheck source=test/e2e/driver.sh
source "$SCRIPT_DIR/driver.sh"
# Fresh-cluster mode: this driver has cluster-wide registration and fixed host
# paths. Refuse to replace an existing installation, even in another namespace.
# Cleanup uses ownership selectors for cluster-wide resources and driver
# objects. A caller-supplied run ID must not adopt remnants of an earlier run.
for resource in "namespace,pv,csidriver -l $OWNER_LABEL=$E2E_RUN_ID" "daemonset,configmap -n kube-system -l $OWNER_LABEL=$E2E_RUN_ID" "csidriver/iscsi.csi.k8s.io" "daemonset/csi-iscsi-node -n kube-system" "daemonsets.apps -A -l app=csi-iscsi-node" "configmap/configmap-csi-iscsiadm -n kube-system" "namespace/$NAMESPACE" "pv/$PV_NAME"; do
    read -r -a resource_args <<< "$resource"
    existing=$(kubectl get "${resource_args[@]}" --ignore-not-found -o name)
    if [[ -n "$existing" ]]; then
        echo "Refusing to reuse existing resource: $existing" >&2; exit 1
    fi
done
prepare_driver
# shellcheck source=test/e2e/target.sh
source "$SCRIPT_DIR/target.sh"
prepare_target
# Label rendered local driver objects so cleanup can select only this run.
python3 - "$DRIVER_MANIFEST_DIR" "$E2E_RUN_ID" "$CLEANUP_TOKEN" <<'LABELS'
import pathlib, sys
for path in pathlib.Path(sys.argv[1]).glob('*.yaml'):
    text = path.read_text().replace('\nmetadata:\n', '\nmetadata:\n  labels:\n    iscsi.csi.k8s.io/e2e-run: "' + sys.argv[2] + '"\n    iscsi.csi.k8s.io/e2e-invocation: "' + sys.argv[3] + '"\n')
    path.write_text(text)
LABELS
E2E_DRIVER_CREATE_ONLY=1
CLEANUP_CLUSTER=1
cat <<EOF | kubectl create -f -
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
EOF

STAGE=node-prerequisites
# Step 0: Validate the existing node setup without installing or starting anything.
echo "[0/7] Checking node iSCSI prerequisites (read-only)..."
NODE_CHECK=$(sed 's/^/                  /' "$SCRIPT_DIR/node-prerequisites.sh")
cat <<EOF | kubectl create -f -
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: $INIT_NAME
  namespace: $NAMESPACE
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  selector:
    matchLabels:
      app: $INIT_NAME
  template:
    metadata:
      labels:
        app: $INIT_NAME
    spec:
      hostPID: true
      hostNetwork: true
      nodeSelector:
        kubernetes.io/os: linux
      containers:
        - name: prerequisites
          image: ubuntu:22.04
          securityContext:
            privileged: true
          command:
            - /bin/sh
            - -c
            - |
              exec sleep infinity
          readinessProbe:
            exec:
              command:
                - chroot
                - /host
                - /bin/sh
                - -c
                - |
$NODE_CHECK
            initialDelaySeconds: 1
            periodSeconds: 5
            timeoutSeconds: 5
          volumeMounts:
            - name: host-root
              mountPath: /host
              readOnly: true
      volumes:
        - name: host-root
          hostPath:
            path: /
EOF
echo "Waiting for node prerequisite checks..."
if kubectl rollout status daemonset/"$INIT_NAME" -n "$NAMESPACE" --timeout=60s; then
    prerequisite_status=$(kubectl get daemonset "$INIT_NAME" -n "$NAMESPACE" -o json)
    python3 - "$prerequisite_status" <<'NODE_STATUS'
import json, sys
status = json.loads(sys.argv[1]).get('status', {})
desired = status.get('desiredNumberScheduled', 0)
if desired < 1 or status.get('numberReady', 0) != desired:
    raise SystemExit('Node prerequisite validation requires at least one eligible node and all validators ready')
NODE_STATUS
    echo 'Node prerequisites verified; no packages or services changed.'
else
    prerequisite_result=$?
    # This describes only our fixed validator's readiness errors. Do not fetch
    # arbitrary application/driver logs or manifests that can contain secrets.
    echo 'Node prerequisite validation failed; inspect readiness events for this validator:' >&2
    kubectl get events -n "$NAMESPACE" --field-selector "involvedObject.kind=Pod" \
        -o 'custom-columns=REASON:.reason,MESSAGE:.message' >&2 || true
    exit "$prerequisite_result"
fi

STAGE=target-rollout
# Step 1: Deploy iSCSI target server
echo "[1/7] Deploying iSCSI target server..."
TARGET_MANIFEST=$(mktemp)
python3 - "$REPO_ROOT/deploy/example/iscsi-server.yaml" "$TARGET_MANIFEST" "$TARGET_NAME" "$NAMESPACE" "$E2E_RUN_ID" "$IQN" "$CLEANUP_TOKEN" "$E2E_TARGET_IMAGE" "$TARGET_PULL_POLICY" <<'TARGET'
import pathlib, re, sys
source, output, name, namespace, run_id, iqn, token, image, policy = sys.argv[1:]
text = pathlib.Path(source).read_text().replace('name: iscsi-server', 'name: ' + name).replace('app: iscsi-server', 'app: ' + name)
text = text.replace('name: ' + name + '\n', 'name: ' + name + '\n  namespace: ' + namespace + '\n')
text = text.replace('iqn.2026-01.com.test:storage', iqn)
if text.count('image: iscsi-e2e-target:local') != 1:
    raise SystemExit('Target manifest changed; update e2e renderer')
text = text.replace('image: iscsi-e2e-target:local', 'image: ' + image).replace('imagePullPolicy: Never', 'imagePullPolicy: ' + policy)
text = re.sub(r'(?m)^  labels:\n', '  labels:\n    iscsi.csi.k8s.io/e2e-run: "' + run_id + '"\n    iscsi.csi.k8s.io/e2e-invocation: "' + token + '"\n', text)
pathlib.Path(output).write_text(text)
TARGET
kubectl create -f "$TARGET_MANIFEST"
kubectl rollout status deployment/"$TARGET_NAME" -n "${NAMESPACE}" --timeout=120s
echo "iSCSI target server is ready."
target_pods=$(kubectl get pods -n "$NAMESPACE" -l "app=$TARGET_NAME" -o json)
# tgtd advertises its PodIP during SendTargets discovery. Using the ServiceIP
# here creates a different node record from the one login/device lookup uses.
TARGET_HOST=$(python3 - "$target_pods" "$E2E_TARGET_IMAGE" "$TARGET_EXPECTED_ID" "$E2E_DRIVER_DELIVERY" <<'ADDRESS'
import ipaddress, json, subprocess, sys
payload, image, expected, delivery = sys.argv[1:]
pods = [pod for pod in json.loads(payload).get('items', [])
        if not pod.get('metadata', {}).get('deletionTimestamp')]
if len(pods) != 1:
    raise SystemExit('Expected exactly one ready target pod')
pod = pods[0]
ready = any(condition.get('type') == 'Ready' and condition.get('status') == 'True'
            for condition in pod.get('status', {}).get('conditions', []))
container = next((c for c in pod.get('spec', {}).get('containers', []) if c['name'] == 'tgtd'), {})
status = next((c for c in pod.get('status', {}).get('containerStatuses', []) if c['name'] == 'tgtd'), {})
runtime_id = status.get('imageID', '')
actual = runtime_id.rsplit('@', 1)[-1].split('://')[-1]
if actual != expected and delivery == 'kind' and runtime_id:
    node = pod.get('spec', {}).get('nodeName', '')
    if node:
        inspection = subprocess.check_output(['docker', 'exec', node, 'crictl', 'inspecti', '-o', 'json', runtime_id], text=True)
        actual = json.loads(inspection)['status']['id']
if not ready or not status.get('ready') or container.get('image') != image or actual != expected:
    raise SystemExit('Target pod is not ready with the selected built image')
print(ipaddress.ip_address(pod['status']['podIP']))
ADDRESS
)
if [[ "$TARGET_HOST" == *:* ]]; then TARGET_PORTAL="[$TARGET_HOST]:3260"; else TARGET_PORTAL="$TARGET_HOST:3260"; fi

STAGE=connectivity
# Step 2: Verify iSCSI target is accessible
echo "[2/7] Verifying iSCSI target is accessible..."
# shellcheck source=test/e2e/checks.sh
source "$SCRIPT_DIR/checks.sh"
check_target_connectivity "$TARGET_HOST" 3260
echo "iSCSI target connectivity check complete."

STAGE=driver-install
# Step 3: Install CSI driver
echo "[3/7] Installing iSCSI CSI driver..."
install_driver
echo "Waiting for CSI driver DaemonSet to be ready..."
kubectl rollout status daemonset/csi-iscsi-node -n kube-system --timeout=120s
echo "iSCSI CSI driver installed."

STAGE=pv-create
# Step 4: Create PV
echo "[4/7] Creating PersistentVolume..."
cat <<EOF | kubectl create -f -
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${PV_NAME}
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: iscsi.csi.k8s.io
    volumeHandle: ${PV_NAME}
    volumeAttributes:
      targetPortal: "${TARGET_PORTAL}"
      iqn: "${IQN}"
      lun: "${LUN}"
      portals: "[]"
      discovery: "true"
EOF
echo "PV created."

STAGE=pvc-create
# Step 5: Create PVC
echo "[5/7] Creating PersistentVolumeClaim..."
cat <<EOF | kubectl create -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
  namespace: ${NAMESPACE}
  labels:
    $OWNER_LABEL: "$E2E_RUN_ID"
    $CLEANUP_LABEL: "$CLEANUP_TOKEN"
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: ${PV_NAME}
EOF
echo "PVC created."

STAGE=pod-create
# Step 6: Create test pod
echo "[6/7] Creating test pod..."
# Arm before create: an API timeout can occur after the pod was accepted.
VOLUME_MAY_BE_PUBLISHED=1
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
      command: ["sh", "-c", "echo 'iSCSI CSI test successful' > /mnt/test-file && cat /mnt/test-file && sleep 3600"]
      volumeMounts:
        - name: iscsi-vol
          mountPath: /mnt
  volumes:
    - name: iscsi-vol
      persistentVolumeClaim:
        claimName: ${PVC_NAME}
EOF

echo "Waiting for test pod to be ready..."
kubectl wait --for=condition=ready pod/"${POD_NAME}" -n "${NAMESPACE}" --timeout=300s
echo "Test pod is ready."

STAGE=data-verification
# Step 7: Verify
echo "[7/7] Verifying mount..."
RESULT=$(kubectl exec "${POD_NAME}" -n "${NAMESPACE}" -- cat /mnt/test-file)
if [ "$RESULT" = "iSCSI CSI test successful" ]; then
  EXIT_CODE=0
else
  echo "E2E data verification failed: unexpected file contents" >&2
  failure_diagnostics 1
fi

exit $EXIT_CODE
