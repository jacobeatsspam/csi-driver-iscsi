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

NAMESPACE="${NAMESPACE:-default}"
TARGET_PORTAL="iscsi-server.${NAMESPACE}.svc.cluster.local:3260"
IQN="iqn.2026-01.com.test:storage"
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

echo "=== iSCSI CSI Driver E2E Test ==="

# Build and deliver the checkout before mutating Kubernetes resources.
# shellcheck source=test/e2e/driver.sh
source "$SCRIPT_DIR/driver.sh"
prepare_driver

STAGE=node-preparation
# Step 0: Ensure open-iscsi is installed on all nodes
echo "[0/7] Ensuring open-iscsi is installed on cluster nodes..."
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: iscsi-init
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: iscsi-init
  template:
    metadata:
      labels:
        app: iscsi-init
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
kubectl rollout status daemonset/iscsi-init -n kube-system --timeout=300s
# Give iscsid a moment to start
sleep 5
echo "open-iscsi installed on all nodes."

STAGE=target-rollout
# Step 1: Deploy iSCSI target server
echo "[1/7] Deploying iSCSI target server..."
kubectl apply -f "${REPO_ROOT}/deploy/example/iscsi-server.yaml"
kubectl rollout status deployment/iscsi-server -n "${NAMESPACE}" --timeout=120s
echo "iSCSI target server is ready."

STAGE=connectivity
# Step 2: Verify iSCSI target is accessible
echo "[2/7] Verifying iSCSI target is accessible..."
# shellcheck source=test/e2e/checks.sh
source "$SCRIPT_DIR/checks.sh"
check_target_connectivity "iscsi-server.${NAMESPACE}.svc.cluster.local" 3260
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
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolume
metadata:
  name: iscsi-e2e-test-pv
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Delete
  csi:
    driver: iscsi.csi.k8s.io
    volumeHandle: iscsi-e2e-test-pv
    volumeAttributes:
      targetPortal: "${TARGET_PORTAL}"
      iqn: "${IQN}"
      lun: "${LUN}"
      portals: "[]"
EOF
echo "PV created."

STAGE=pvc-create
# Step 5: Create PVC
echo "[5/7] Creating PersistentVolumeClaim..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: iscsi-e2e-test-pvc
  namespace: ${NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: iscsi-e2e-test-pv
EOF
echo "PVC created."

STAGE=pod-create
# Step 6: Create test pod
echo "[6/7] Creating test pod..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: iscsi-e2e-test-pod
  namespace: ${NAMESPACE}
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
        claimName: iscsi-e2e-test-pvc
EOF

echo "Waiting for test pod to be ready..."
kubectl wait --for=condition=ready pod/iscsi-e2e-test-pod -n "${NAMESPACE}" --timeout=300s
echo "Test pod is ready."

STAGE=data-verification
# Step 7: Verify
echo "[7/7] Verifying mount..."
RESULT=$(kubectl exec iscsi-e2e-test-pod -n "${NAMESPACE}" -- cat /mnt/test-file)
if [ "$RESULT" = "iSCSI CSI test successful" ]; then
  echo "✅ E2E test PASSED: iSCSI volume mounted and writable"
  EXIT_CODE=0
else
  echo "E2E data verification failed: unexpected file contents" >&2
  failure_diagnostics 1
fi

# Cleanup
echo "Cleaning up..."
kubectl delete pod iscsi-e2e-test-pod -n "${NAMESPACE}" --grace-period=0 --force 2>/dev/null || true
kubectl delete pvc iscsi-e2e-test-pvc -n "${NAMESPACE}" 2>/dev/null || true
kubectl delete pv iscsi-e2e-test-pv 2>/dev/null || true
kubectl delete -f "${REPO_ROOT}/deploy/example/iscsi-server.yaml" 2>/dev/null || true
kubectl delete daemonset iscsi-init -n kube-system 2>/dev/null || true
"${REPO_ROOT}/deploy/uninstall-driver.sh" 2>/dev/null || true

exit $EXIT_CODE
