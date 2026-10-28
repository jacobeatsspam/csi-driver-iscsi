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

# driver.sh supplies the already validated delivery/cluster selection and shared
# pushed_image_ref helper. Always build this checkout's target, never pull latest.
prepare_target() {
    case ${E2E_DRIVER_DELIVERY:-} in
        kind) : "${E2E_KIND_CLUSTER:?Set E2E_KIND_CLUSTER for target delivery}" ;;
        registry) ;;
        *) echo 'Target delivery requires validated kind or registry mode' >&2; return 1 ;;
    esac
    if [[ ${E2E_DRIVER_DELIVERY:-kind} == registry && -z ${E2E_TARGET_IMAGE:-} ]]; then
        echo 'Set E2E_TARGET_IMAGE to an explicit test-registry repository:tag' >&2; return 1
    fi
    E2E_TARGET_IMAGE=${E2E_TARGET_IMAGE:-iscsi-e2e-target:${DRIVER_SOURCE_REVISION:0:12}-$(date +%s)-$$}
    if [[ "$E2E_TARGET_IMAGE" =~ [[:space:]] || "${E2E_TARGET_IMAGE##*/}" != *:* || "$E2E_TARGET_IMAGE" == *@* ||
          ${E2E_TARGET_IMAGE##*:} == latest || ${E2E_TARGET_IMAGE##*:} == canary ]]; then
        echo 'E2E_TARGET_IMAGE must have an explicit non-latest/non-canary tag' >&2; return 1
    fi
    docker build -t "$E2E_TARGET_IMAGE" "$REPO_ROOT/deploy/example/tgtd" || return
    TARGET_LOCAL_ID=$(docker image inspect --format '{{.Id}}' "$E2E_TARGET_IMAGE") || return
    if [[ ! "$TARGET_LOCAL_ID" =~ ^sha256:[a-f0-9]{64}$ ]]; then
        echo 'Cannot determine locally built target image ID' >&2; return 1
    fi
    if [[ "$E2E_DRIVER_DELIVERY" == kind ]]; then
        kind load docker-image "$E2E_TARGET_IMAGE" --name "$E2E_KIND_CLUSTER" || return
        TARGET_PULL_POLICY=Never
        TARGET_EXPECTED_ID=$TARGET_LOCAL_ID
    else
        docker push "$E2E_TARGET_IMAGE" || return
        E2E_TARGET_IMAGE=$(pushed_image_ref "$E2E_TARGET_IMAGE") || return
        TARGET_PULL_POLICY=Always
        TARGET_EXPECTED_ID=${E2E_TARGET_IMAGE##*@}
    fi
    export TARGET_PULL_POLICY TARGET_EXPECTED_ID
    echo "Target image: $E2E_TARGET_IMAGE; local ID: $TARGET_LOCAL_ID"
}

# Verify the current ready target image and return its advertised address.
target_endpoint() {
# tgtd advertises its PodIP during SendTargets discovery. Using the ServiceIP
# here creates a different node record from the one login/device lookup uses.
python3 - "$NAMESPACE" "$TARGET_NAME" "$E2E_TARGET_IMAGE" "$TARGET_EXPECTED_ID" "$E2E_DRIVER_DELIVERY" "${E2E_COMMAND_TIMEOUT:-330}" "${E2E_REQUEST_TIMEOUT:-30}" <<'ADDRESS' || return
import ipaddress, json, subprocess, sys, time
namespace, name, image, expected, delivery, command_timeout, request_timeout = sys.argv[1:]
command_timeout, request_timeout = int(command_timeout), int(request_timeout)
if min(command_timeout, request_timeout) <= 0:
    raise SystemExit("Image identity timeouts must be positive")
deadline = time.monotonic() + command_timeout

def bounded_output(args):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise SystemExit('Timed out resolving ' + delivery + ' target image identity')
    try:
        return subprocess.check_output(args, text=True, timeout=remaining)
    except subprocess.TimeoutExpired:
        raise SystemExit('Timed out resolving ' + delivery + ' target image identity')
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.returncode)

payload = bounded_output(['kubectl', '--request-timeout=%ss' % min(request_timeout, command_timeout),
                          'get', 'pods', '-n', namespace, '-l', 'app=' + name, '-o', 'json'])
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
        inspection = bounded_output(['docker', 'exec', node, 'crictl', 'inspecti', '-o', 'json', runtime_id])
        actual = json.loads(inspection)['status']['id']
if not ready or not status.get('ready') or container.get('image') != image or actual != expected:
    raise SystemExit('Target pod is not ready with the selected built image')
if time.monotonic() > deadline:
    raise SystemExit('Timed out resolving ' + delivery + ' target image identity')
print(ipaddress.ip_address(pod['status']['podIP']))
ADDRESS
}
