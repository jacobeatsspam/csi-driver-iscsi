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

# Run in the node root with hostPID. /proc/1/mountinfo describes the node's mount
# namespace; /proc/self/mountinfo would describe the validator container instead.
set -eu
mode=$1 iqn=$2 volume=$3 tracked=${4:-} node_root=${5:-/} lun=${6:-}
case "$mode" in snapshot|check|snapshot-lun|check-lun|published) ;; *) echo 'Invalid cleanup check mode' >&2; exit 2 ;; esac
if [ "$mode" = snapshot-lun ] || [ "$mode" = check-lun ]; then
    case "$lun" in ''|*[!0-9]*) echo 'Invalid LUN identifier' >&2; exit 2 ;; esac
fi
case "$volume" in ''|*[!a-zA-Z0-9_-]*) echo 'Invalid cleanup volume identifier' >&2; exit 2 ;; esac
[ -r "$node_root/proc/1/mountinfo" ] || { echo 'Cannot observe node mount namespace' >&2; exit 2; }
[ -d "$node_root/sys/class/block" ] || { echo 'Cannot observe node block devices' >&2; exit 2; }

if [ "$mode" = published ]; then
    if awk -v marker="/volumes/kubernetes.io~csi/$volume/" 'index($5, marker) { found=1 } END { exit !found }' "$node_root/proc/1/mountinfo"; then exit 0; fi
    echo 'Expected A publication mount is not observable' >&2
    exit 2
fi

residual=0
for target in "$node_root"/sys/class/iscsi_session/session*/targetname; do
    [ -e "$target" ] || continue
    [ -r "$target" ] || { echo 'Cannot observe iSCSI session identity' >&2; exit 2; }
    IFS= read -r target_iqn < "$target"
    [ "$target_iqn" = "$iqn" ] || continue
    if [ "$mode" = snapshot ] || [ "$mode" = snapshot-lun ]; then
        for block in "${target%/targetname}"/device/target*/*/block/*; do
            [ -e "$block" ] || continue
            if [ "$mode" = snapshot-lun ]; then
                address=${block%/block/*}; address=${address##*/}
                [ "${address##*:}" = "$lun" ] || continue
            fi
            basename "$block"
        done
    elif [ "$mode" = check ]; then
        echo 'Run iSCSI session remains on node' >&2
        residual=1
    fi
done
[ "$mode" = check ] || [ "$mode" = check-lun ] || exit 0

if awk -v marker="/volumes/kubernetes.io~csi/$volume/" 'index($5, marker) { found=1 } END { exit !found }' "$node_root/proc/1/mountinfo"; then
    echo 'Run publication mount remains in node mount namespace' >&2
    residual=1
else
    result=$?
    [ "$result" = 1 ] || { echo 'Cannot read node mount namespace' >&2; exit 2; }
fi
for path in "$node_root"/dev/disk/by-path/*-iscsi-"$iqn"-lun-*; do
    if [ "$mode" = check-lun ] && [ "${path##*-lun-}" != "$lun" ]; then continue; fi
    if [ -e "$path" ] || [ -L "$path" ]; then
        echo 'Run iSCSI device path remains on node' >&2
        residual=1
    fi
done
# Deliberate word splitting: snapshot contains Linux block names, validated
# before use. An old block device must not disappear from observation merely
# because its session has already been removed.
set -f
for name in $tracked; do
    case "$name" in ''|*[!a-zA-Z0-9_-]*) echo 'Invalid tracked block device' >&2; exit 2 ;; esac
    if [ -e "$node_root/sys/class/block/$name" ]; then
        echo 'Previously observed run block device remains on node' >&2
        residual=1
    fi
done
exit "$residual"
