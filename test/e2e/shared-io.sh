#!/bin/sh
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


# Runs only in the disposable B workload. Every completed iteration includes a
# forced writeback and checked read; eventual reconnection cannot erase failure.
set -eu
fail() { echo 'Shared target B I/O failed' >&2; exit 1; }
data_dir=${SHARED_DATA_DIR:-/mnt}
progress_file=${SHARED_PROGRESS_FILE:-/tmp/io-progress}
io_timeout=${SHARED_IO_TIMEOUT:-10}
case "$io_timeout" in ''|*[!0-9]*|0) fail ;; esac
expected='iSCSI shared target B payload'
# Bound initialization as well as every later target-file operation.
# shellcheck disable=SC2016
timeout -k 1 "$io_timeout" sh -c 'printf "%s\n" "$2" > "$1"' seed "$data_dir/b-pattern" "$expected" || fail
timeout -k 1 "$io_timeout" sync || fail
count=0
while :; do
    actual=$(timeout -k 1 "$io_timeout" cat "$data_dir/b-pattern") || fail
    [ "$actual" = "$expected" ] || fail
    timeout -k 1 "$io_timeout" dd if="$data_dir/b-pattern" of="$data_dir/b-io" conv=fsync 2>/dev/null || fail
    timeout -k 1 "$io_timeout" cmp "$data_dir/b-pattern" "$data_dir/b-io" >/dev/null 2>&1 || fail
    count=$((count + 1))
    printf '%s\n' "$count" > "$progress_file.next"
    mv "$progress_file.next" "$progress_file"
    sleep 1
done
