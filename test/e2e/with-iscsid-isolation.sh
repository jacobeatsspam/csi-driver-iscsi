#!/usr/bin/env bash

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

lock_only=0
if [[ ${1:-} == --lock-only ]]; then
  lock_only=1
  shift
fi

# Registry delivery can target a remote cluster, so the local host is irrelevant.
if [[ $lock_only = 0 && ${E2E_DRIVER_DELIVERY:-kind} != kind ]]; then
  exec "$@"
fi

if [ "$#" -eq 0 ]; then
  echo "Provide the test command." >&2
  exit 2
fi
if ! command -v systemctl >/dev/null; then
  echo "kind iSCSI tests require systemd to isolate the host iscsid service." >&2
  exit 1
fi
sudo_cmd=()
if [ "$(id -u)" -ne 0 ]; then
  sudo_cmd=(sudo)
fi
systemd() { "${sudo_cmd[@]}" systemctl "$@"; }

# Serialize all kind tests on this host so one cannot unmask another's service.
lock_dir=/run/lock/csi-iscsi-e2e
if [ -L "$lock_dir" ]; then
  echo "Refusing a symlinked iSCSI test lock directory." >&2
  exit 1
fi
"${sudo_cmd[@]}" install -d -m 0755 "$lock_dir"
if [ "$(stat -c %u "$lock_dir")" != 0 ] || [ -L "$lock_dir/iscsid.lock" ]; then
  echo "Unsafe iSCSI test lock directory." >&2
  exit 1
fi
if [ -e "$lock_dir/iscsid.lock" ] &&
   { [ ! -f "$lock_dir/iscsid.lock" ] || [ "$(stat -c %u "$lock_dir/iscsid.lock")" != 0 ]; }; then
  echo "Unsafe iSCSI test lock file." >&2
  exit 1
fi
"${sudo_cmd[@]}" touch "$lock_dir/iscsid.lock"
"${sudo_cmd[@]}" chmod 0666 "$lock_dir/iscsid.lock"
if ! [ "/proc/$$/fd/9" -ef "$lock_dir/iscsid.lock" ]; then
  exec 9<>"$lock_dir/iscsid.lock"
fi
if ! flock -n 9; then
  echo "Another kind iSCSI test owns the host lock." >&2
  exit 1
fi

if [ "$lock_only" = 1 ]; then
  exec "$@"
fi

sessions_empty() {
  [ -d /sys/class/iscsi_session ] || return 1
  local sessions
  sessions=$(ls -A /sys/class/iscsi_session) || return 1
  [ -z "$sessions" ]
}

# Never stop an existing initiator or disrupt a host already using iSCSI.
if [ "$(systemd show iscsid.service --property=ActiveState --value)" != inactive ] ||
   ! sessions_empty; then
  echo "Host iscsid is active or iSCSI session state is not empty and observable; refusing to run kind tests." >&2
  exit 1
fi
service_masked=0
restore() {
  status=$?
  trap - EXIT
  if [ "$service_masked" = 1 ]; then
    if [ "$status" -eq 0 ] && sessions_empty; then
      systemd unmask --runtime iscsid.service || status=1
    else
      echo "Host iscsid remains runtime-masked because the test failed or iSCSI sessions remain or cannot be observed." >&2
      echo "After removing retained test workloads and confirming all iSCSI sessions are gone, run: sudo systemctl unmask --runtime iscsid.service" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap restore EXIT

service_state=$(systemd is-enabled iscsid.service 2>/dev/null || true)
if [[ $service_state != masked && $service_state != masked-runtime ]]; then
  systemd mask --runtime iscsid.service
  service_masked=1
fi
# Masking does not stop a daemon that started concurrently.
if [ "$(systemd show iscsid.service --property=ActiveState --value)" != inactive ] || ! sessions_empty; then
  echo "Host iscsid or its session state changed during setup; refusing to run kind tests." >&2
  exit 1
fi
"$@"
