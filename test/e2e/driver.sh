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

# shellcheck source=test/e2e/images.sh
source "$REPO_ROOT/test/e2e/images.sh"

# Local-build installation used by the e2e harness. Sourced, not executable.
prepare_driver() {
    DRIVER_SOURCE_REVISION=$(git -C "$REPO_ROOT" rev-parse HEAD) || return
    E2E_DRIVER_IMAGE=${E2E_DRIVER_IMAGE:-iscsi-e2e:${DRIVER_SOURCE_REVISION:0:12}-$(date +%s)-$$}
    E2E_DRIVER_DELIVERY=${E2E_DRIVER_DELIVERY:-kind}
    E2E_KIND_CLUSTER=${E2E_KIND_CLUSTER:-}
    E2E_ARCH=${E2E_ARCH:-$(go env GOARCH)}
    if [[ "$E2E_DRIVER_IMAGE" =~ [[:space:]] ]] ||
       [[ "${E2E_DRIVER_IMAGE##*/}" != *:* || "$E2E_DRIVER_IMAGE" == *@* ]] ||
       [[ "${E2E_DRIVER_IMAGE##*:}" == latest || "${E2E_DRIVER_IMAGE##*:}" == canary ]]; then
        echo 'E2E_DRIVER_IMAGE must have an explicit non-latest/non-canary tag' >&2
        return 1
    fi
    case "$E2E_DRIVER_DELIVERY" in
        kind)
            : "${E2E_KIND_CLUSTER:?Set E2E_KIND_CLUSTER to the disposable kind cluster}"
            if [[ "$(kubectl config current-context)" != "kind-$E2E_KIND_CLUSTER" ]]; then
                echo 'kubectl context does not match E2E_KIND_CLUSTER' >&2; return 1
            fi
            ;;
        registry) ;;
        *) echo 'E2E_DRIVER_DELIVERY must be kind or registry' >&2; return 1 ;;
    esac
    # Always build this checkout: selecting a tag must not select a stale image.
    make -C "$REPO_ROOT" iscsi ARCH="$E2E_ARCH" || return
    docker build --build-arg "ARCH=$E2E_ARCH" -t "$E2E_DRIVER_IMAGE" "$REPO_ROOT" || return
    DRIVER_LOCAL_ID=$(docker image inspect --format '{{.Id}}' "$E2E_DRIVER_IMAGE") || return
    if [[ ! "$DRIVER_LOCAL_ID" =~ ^sha256:[a-f0-9]{64}$ ]]; then
        echo 'Cannot determine locally built driver image ID' >&2; return 1
    fi
    if [[ "$E2E_DRIVER_DELIVERY" == kind ]]; then
        kind load docker-image "$E2E_DRIVER_IMAGE" --name "$E2E_KIND_CLUSTER" || return
        DRIVER_EXPECTED_ID=$DRIVER_LOCAL_ID
        DRIVER_PULL_POLICY=Never
    else
        docker push "$E2E_DRIVER_IMAGE" || return
        DRIVER_DIGEST_REF=$(pushed_image_ref "$E2E_DRIVER_IMAGE") || return
        E2E_DRIVER_IMAGE=$DRIVER_DIGEST_REF
        DRIVER_EXPECTED_ID=${DRIVER_DIGEST_REF##*@}
        DRIVER_PULL_POLICY=Always
    fi
    DRIVER_MANIFEST_DIR=$(mktemp -d) || return
    cp "$REPO_ROOT/deploy/csi-iscsi-driverinfo.yaml" "$DRIVER_MANIFEST_DIR/" || return
    python3 - "$REPO_ROOT/deploy/csi-iscsi-node.yaml" "$DRIVER_MANIFEST_DIR/csi-iscsi-node.yaml" "$E2E_DRIVER_IMAGE" "$DRIVER_PULL_POLICY" <<'PY' || return
import pathlib, sys
source, output, image, policy = sys.argv[1:]
text = pathlib.Path(source).read_text()
old = 'image: gcr.io/k8s-staging-sig-storage/iscsiplugin:canary'
if text.count(old) != 1 or text.count('imagePullPolicy: "IfNotPresent"') != 1:
    raise SystemExit('Local driver manifest changed; update the e2e renderer')
text = text.replace(old, 'image: ' + image).replace('imagePullPolicy: "IfNotPresent"', 'imagePullPolicy: ' + policy)
pathlib.Path(output).write_text(text)
PY
    echo "Driver source: $DRIVER_SOURCE_REVISION (working-tree changes included)"
    echo "Driver image: $E2E_DRIVER_IMAGE; local ID: $DRIVER_LOCAL_ID; expected runtime ID: $DRIVER_EXPECTED_ID"
}

install_driver() {
    local operation=apply
    if [[ ${E2E_DRIVER_CREATE_ONLY:-0} == 1 ]]; then operation=create; fi
    kubectl "$operation" -f "$DRIVER_MANIFEST_DIR/csi-iscsi-driverinfo.yaml" || return
    kubectl "$operation" -f "$DRIVER_MANIFEST_DIR/csi-iscsi-node.yaml" || return
    kubectl rollout status daemonset/csi-iscsi-node -n kube-system --timeout=120s || return
    verify_driver || return
    rm -rf "$DRIVER_MANIFEST_DIR"
}

verify_driver() {
    python3 - "$E2E_DRIVER_IMAGE" "$DRIVER_EXPECTED_ID" "$E2E_DRIVER_DELIVERY" "${E2E_COMMAND_TIMEOUT:-330}" "${E2E_REQUEST_TIMEOUT:-30}" <<'PY' || return
import json, subprocess, sys, time
image, expected, delivery, command_timeout, request_timeout = sys.argv[1:]
command_timeout, request_timeout = int(command_timeout), int(request_timeout)
if min(command_timeout, request_timeout) <= 0:
    raise SystemExit("Image identity timeouts must be positive")
deadline = time.monotonic() + command_timeout

def bounded_output(args):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise SystemExit('Timed out resolving ' + delivery + ' driver image identity')
    try:
        return subprocess.check_output(args, text=True, timeout=remaining)
    except subprocess.TimeoutExpired:
        raise SystemExit('Timed out resolving ' + delivery + ' driver image identity')
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.returncode)

payload = bounded_output(['kubectl', '--request-timeout=%ss' % min(request_timeout, command_timeout),
                          'get', 'pods', '-n', 'kube-system', '-l', 'app=csi-iscsi-node', '-o', 'json'])
pods = [pod for pod in json.loads(payload)['items'] if not pod.get('metadata', {}).get('deletionTimestamp')]
if not pods:
    raise SystemExit('No driver pods found after rollout')
for pod in pods:
    spec = next(c for c in pod['spec']['containers'] if c['name'] == 'iscsi')
    status = next((c for c in pod.get('status', {}).get('containerStatuses', []) if c['name'] == 'iscsi'), {})
    runtime_id = status.get('imageID', '')
    # CRI runtimes report either a bare digest, a scheme-prefixed digest or
    # a repository@digest. Match the full digest, never just the mutable tag.
    actual = runtime_id.rsplit('@', 1)[-1].split('://')[-1]
    if actual != expected and delivery == 'kind' and runtime_id:
        # containerd can report a manifest digest instead of Docker's config ID.
        # Resolve that exact running image on its kind node, not its mutable tag.
        node = pod['spec'].get('nodeName', '')
        if node:
            inspection = bounded_output(['docker', 'exec', node, 'crictl', 'inspecti', '-o', 'json', runtime_id])
            actual = json.loads(inspection)['status']['id']
    if spec['image'] != image or not status.get('ready') or actual != expected:
        raise SystemExit('Driver pod %s does not run the selected ready image: %s' % (pod['metadata']['name'], runtime_id))
    if time.monotonic() > deadline:
        raise SystemExit('Timed out resolving ' + delivery + ' driver image identity')
    print('Verified driver pod %s imageID=%s' % (pod['metadata']['name'], runtime_id))
PY
}
