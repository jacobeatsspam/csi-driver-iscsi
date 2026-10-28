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
for duration in "$E2E_COMMAND_TIMEOUT" "$E2E_REQUEST_TIMEOUT"; do
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

STAGE=node-preparation
# Step 0: Ensure open-iscsi is installed on all nodes
echo "[0/7] Ensuring open-iscsi is installed on cluster nodes..."
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
        - name: iscsi-init
          image: ubuntu:22.04
          securityContext:
            privileged: true
          command:
            - nsenter
            - --target
            - "1"
            - --mount
            - --uts
            - --ipc
            - --net
            - --pid
            - --
            - bash
            - -c
            - |
              if ! command -v iscsiadm &>/dev/null; then
                echo "Installing open-iscsi..."
                apt-get update -qq && apt-get install -y -qq open-iscsi >/dev/null 2>&1 || \
                yum install -y -q iscsi-initiator-utils >/dev/null 2>&1 || \
                echo "WARNING: Could not install open-iscsi"
              fi
              # Ensure iscsid is running
              if command -v systemctl &>/dev/null; then
                systemctl enable iscsid --now 2>/dev/null || true
              else
                iscsid 2>/dev/null || true
              fi
              echo "open-iscsi ready"
              sleep infinity
          volumeMounts:
            - name: host-root
              mountPath: /host
      volumes:
        - name: host-root
          hostPath:
            path: /
EOF
echo "Waiting for iscsi-init DaemonSet to be ready..."
kubectl rollout status daemonset/"$INIT_NAME" -n "$NAMESPACE" --timeout=300s
# Give iscsid a moment to start
sleep 5
echo "open-iscsi installed on all nodes."

STAGE=target-rollout
# Step 1: Deploy iSCSI target server
echo "[1/7] Deploying iSCSI target server..."
TARGET_MANIFEST=$(mktemp)
python3 - "$REPO_ROOT/deploy/example/iscsi-server.yaml" "$TARGET_MANIFEST" "$TARGET_NAME" "$NAMESPACE" "$E2E_RUN_ID" "$IQN" "$CLEANUP_TOKEN" <<'TARGET'
import pathlib, re, sys
source, output, name, namespace, run_id, iqn, token = sys.argv[1:]
text = pathlib.Path(source).read_text().replace('name: iscsi-server', 'name: ' + name).replace('app: iscsi-server', 'app: ' + name)
text = text.replace('name: ' + name + '\n', 'name: ' + name + '\n  namespace: ' + namespace + '\n')
text = text.replace('iqn.2026-01.com.test:storage', iqn)
text = re.sub(r'(?m)^  labels:\n', '  labels:\n    iscsi.csi.k8s.io/e2e-run: "' + run_id + '"\n    iscsi.csi.k8s.io/e2e-invocation: "' + token + '"\n', text)
pathlib.Path(output).write_text(text)
TARGET
kubectl create -f "$TARGET_MANIFEST"
kubectl rollout status deployment/"$TARGET_NAME" -n "${NAMESPACE}" --timeout=120s
echo "iSCSI target server is ready."
TARGET_HOST=$(kubectl get service "$TARGET_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.clusterIP}')
python3 - "$TARGET_HOST" <<'ADDRESS'
import ipaddress, sys
ipaddress.ip_address(sys.argv[1])
ADDRESS
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
  persistentVolumeReclaimPolicy: Delete
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
