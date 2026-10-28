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

set -eu
IQN=${IQN:-iqn.2026-01.com.test:storage}
DISK_PATH=${DISK_PATH:-/var/lib/iscsi/disk0.img}
TGTD_CONTROL_TIMEOUT=${TGTD_CONTROL_TIMEOUT:-3}
if [[ ! "$TGTD_CONTROL_TIMEOUT" =~ ^[1-9][0-9]?$ ]]; then exit 2; fi
# Without --tid, tgtadm emits the target/LUN inventory. With --tid it emits
# target parameters (not LUNs), which cannot establish protocol readiness.
configuration=$(timeout --foreground --kill-after=1s "${TGTD_CONTROL_TIMEOUT}s" tgtadm --lld iscsi --op show --mode target)
printf '%s\n' "$configuration" | awk -v iqn="$IQN" -v path="$DISK_PATH" '
$1 == "Target" { target=($2 == "1:" && $3 == iqn); lun=""; online=0; backing=0 }
$1 == "LUN:" { lun=$2; online=0; backing=0 }
target && lun == 1 && $1 == "Online:" && $2 == "Yes" { online=1 }
target && lun == 1 && $1 == "Backing" && $2 == "store" && $3 == "path:" {
    sub(/^[ \t]*Backing store path: /, "")
    if ($0 == path) backing=1
}
target && lun == 1 && online && backing { valid=1 }
END { exit !valid }
'
