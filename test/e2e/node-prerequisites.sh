#!/bin/sh
# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Execute inside the node root and PID namespace, not the validator image root.
# Optional root argument permits unprivileged fixture tests of proc/sys/run.
set -eu
node_root=${1:-/}
fail() { echo "Node prerequisite failed: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || fail 'Linux nodes are required'
command -v iscsiadm >/dev/null 2>&1 || fail 'iscsiadm is missing; prepare the disposable node first'

process_running() {
    for comm in "$node_root"/proc/[0-9]*/comm; do
        [ -r "$comm" ] || continue
        IFS= read -r process_name < "$comm" || continue
        [ "$process_name" != "$1" ] || return 0
    done
    return 1
}
process_running iscsid || fail 'iscsid is not running'
[ -d "$node_root/sys/class/iscsi_transport/tcp" ] || fail 'iSCSI TCP kernel transport is unavailable'
if ! process_running systemd-udevd && ! process_running udevd; then
    fail 'udev is not running; device discovery requires udev'
fi
echo 'Node iSCSI prerequisites verified (read-only checks)'
