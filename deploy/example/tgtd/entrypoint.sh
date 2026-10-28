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

set -euo pipefail

DISK_SIZE=${DISK_SIZE:-1024}  # MiB; initialize only a new, disposable backing file.
IQN=${IQN:-iqn.2026-01.com.test:storage}
DISK_PATH=${DISK_PATH:-/var/lib/iscsi/disk0.img}
LUN_COUNT=${LUN_COUNT:-1}
DISK_PATH_B=${DISK_PATH_B:-/var/lib/iscsi/disk1.img}
TGTD_STARTUP_ATTEMPTS=${TGTD_STARTUP_ATTEMPTS:-10}
TGTD_STARTUP_INTERVAL=${TGTD_STARTUP_INTERVAL:-1}
TGTD_CONTROL_TIMEOUT=${TGTD_CONTROL_TIMEOUT:-3}
TGTD_SHUTDOWN_TIMEOUT=${TGTD_SHUTDOWN_TIMEOUT:-3}
if [[ "$LUN_COUNT" != 1 && "$LUN_COUNT" != 2 ]]; then echo 'LUN_COUNT must be 1 or 2' >&2; exit 2; fi
if [[ "$LUN_COUNT" == 2 && "$DISK_PATH" == "$DISK_PATH_B" ]]; then echo 'LUN backing paths must differ' >&2; exit 2; fi
if [[ ! "$DISK_SIZE" =~ ^[1-9][0-9]{0,5}$ || ! "$TGTD_STARTUP_ATTEMPTS" =~ ^[1-9][0-9]{0,2}$ ||
      ! "$TGTD_STARTUP_INTERVAL" =~ ^[0-9]{1,2}$ || ! "$TGTD_CONTROL_TIMEOUT" =~ ^[1-9][0-9]?$ ||
      ! "$TGTD_SHUTDOWN_TIMEOUT" =~ ^[1-9][0-9]?$ || "$IQN" != iqn.* || "$IQN" =~ [[:space:]] ]]; then
    echo 'Invalid target size, IQN or startup bounds' >&2; exit 2
fi
expected_size=$((DISK_SIZE * 1024 * 1024))
initialize_backing() {
local DISK_PATH=$1
if [[ -L "$DISK_PATH" || ( -e "$DISK_PATH" && ! -f "$DISK_PATH" ) ]]; then
    echo 'Backing path must be a regular non-symlink file' >&2; exit 1
fi
if [[ -e "$DISK_PATH" ]]; then
    if [[ $(stat -c %s "$DISK_PATH") != "$expected_size" ]]; then
        echo 'Existing backing file size differs; refusing to overwrite data' >&2; exit 1
    fi
else
    mkdir -p "$(dirname "$DISK_PATH")"
    # Noclobber also rejects a concurrently created file instead of erasing it.
    (set -C; : > "$DISK_PATH")
    truncate -s "$expected_size" "$DISK_PATH"
fi

}
initialize_backing "$DISK_PATH"
if [[ "$LUN_COUNT" == 2 ]]; then initialize_backing "$DISK_PATH_B"; fi

TGTD_PID=
cleanup() {
    local result=$1 deadline=$((SECONDS + TGTD_SHUTDOWN_TIMEOUT))
    if [[ -n "$TGTD_PID" ]]; then
        kill -TERM "$TGTD_PID" 2>/dev/null || true
        while kill -0 "$TGTD_PID" 2>/dev/null && ((SECONDS < deadline)); do sleep 0.1; done
        if kill -0 "$TGTD_PID" 2>/dev/null; then kill -KILL "$TGTD_PID" 2>/dev/null || true; fi
        wait "$TGTD_PID" 2>/dev/null || true
    fi
    return "$result"
}
trap 'cleanup "$?"' EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

control() {
    timeout --foreground --kill-after=1s "${TGTD_CONTROL_TIMEOUT}s" tgtadm --lld iscsi "$@"
}

tgtd -f &
TGTD_PID=$!
ready=0
for ((attempt=0; attempt<TGTD_STARTUP_ATTEMPTS; attempt++)); do
    kill -0 "$TGTD_PID" 2>/dev/null || { echo 'tgtd exited before startup' >&2; exit 1; }
    if control --op show --mode sys >/dev/null 2>&1; then ready=1; break; fi
    sleep "$TGTD_STARTUP_INTERVAL"
done
if [[ "$ready" != 1 ]]; then echo 'Timed out waiting for tgtd control readiness' >&2; exit 1; fi

kill -0 "$TGTD_PID" 2>/dev/null || { echo 'tgtd exited before configuration' >&2; exit 1; }

control --op new --mode target --tid 1 -T "$IQN"
control --op new --mode logicalunit --tid 1 --lun 1 -b "$DISK_PATH"
if [[ "$LUN_COUNT" == 2 ]]; then control --op new --mode logicalunit --tid 1 --lun 2 -b "$DISK_PATH_B"; fi
control --op bind --mode target --tid 1 -I ALL
"$(dirname "${BASH_SOURCE[0]}")/ready.sh"
kill -0 "$TGTD_PID" 2>/dev/null || { echo 'tgtd exited during configuration' >&2; exit 1; }
echo 'iSCSI target configured and ready'
wait "$TGTD_PID"
