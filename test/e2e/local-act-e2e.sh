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

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/csi-local-act.XXXXXXXX")
vm_pid=
log_pid=
cleanup() {
	trap '' INT TERM HUP
	if [ -n "$vm_pid" ]; then
		kill "$vm_pid" 2>/dev/null || true
		wait "$vm_pid" 2>/dev/null || true
	fi
	if [ -n "$log_pid" ]; then
		kill "$log_pid" 2>/dev/null || true
		wait "$log_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

mkdir "$work/seed"
image=${LOCAL_ACT_VM_IMAGE:-${TMPDIR:-/tmp}/csi-local-act-ubuntu-noble-20261017-$(id -u).img}
image_sha=04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94
act=${LOCAL_ACT_BINARY_ARCHIVE:-$work/act.tar.gz}
if [ -z "${LOCAL_ACT_VM_IMAGE:-}" ]; then
	if [ -L "$image" ] || { [ -e "$image" ] && { [ ! -f "$image" ] || [ ! -O "$image" ]; }; }; then
		echo "Refusing unsafe cached VM image: $image" >&2
		exit 1
	fi
	if [ ! -f "$image" ] || ! printf '%s  %s\n' "$image_sha" "$image" | sha256sum -c --status; then
		curl -fL --retry 3 -o "$work/ubuntu.img" https://cloud-images.ubuntu.com/noble/20261017/noble-server-cloudimg-amd64.img
		printf '%s  %s\n' "$image_sha" "$work/ubuntu.img" | sha256sum -c -
		mv -f -- "$work/ubuntu.img" "$image"
	else
		echo "Using cached VM image: $image"
	fi
else
	printf '%s  %s\n' "$image_sha" "$image" | sha256sum -c -
fi
if [ -z "${LOCAL_ACT_BINARY_ARCHIVE:-}" ]; then
	curl -fL --retry 3 -o "$act" https://github.com/nektos/act/releases/download/v0.2.97/act_Linux_x86_64.tar.gz
fi
printf '%s  %s\n' 67b4976a56f33ea7870b798912e738ec44eef38e7578ae7b532f2def1c73a5ad "$act" | sha256sum -c -
cp "$act" "$work/seed/act.tar.gz"
cd "$root"
# Include working-tree changes and untracked files, but not ignored files or .git.
while IFS= read -r -d '' file; do
	if [ -e "$file" ] || [ -L "$file" ]; then printf '%s\0' "$file"; fi
done < <(git ls-files --cached --others --exclude-standard -z) | tar --null -czf "$work/seed/source.tar.gz" -T -
cat >"$work/seed/meta-data" <<'META'
instance-id: csi-local-act
local-hostname: csi-local-act
META
cat >"$work/seed/user-data" <<'CLOUD'
#cloud-config
runcmd:
  - [ mkdir, -p, /mnt/seed ]
  - [ mount, -o, ro, /dev/sr0, /mnt/seed ]
  - [ systemctl, stop, serial-getty@ttyS0.service ]
  - [ sh, -c, 'echo LOCAL_ACT_BEGIN > /dev/ttyS0; touch /tmp/guest.log; bash /mnt/seed/guest.sh > /tmp/guest.log 2>&1 & job=$!; tail -n +1 -f /tmp/guest.log > /dev/ttyS0 & logger=$!; if wait $job; then result=PASS; else result=FAIL; fi; kill $logger || true; wait $logger || true; if [ $result = FAIL ]; then cat /tmp/guest.log > /dev/ttyS0; fi; echo LOCAL_ACT_$result > /dev/ttyS0; poweroff' ]
CLOUD
cat >"$work/seed/guest.sh" <<'GUEST'
#!/bin/bash
set -euxo pipefail
apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 update
DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io docker-buildx git nodejs make curl kmod ca-certificates golang-go
export HOME=/root
systemctl start docker
tar xzf /mnt/seed/act.tar.gz -C /usr/local/bin act
mkdir /root/repo
tar xzf /mnt/seed/source.tar.gz -C /root/repo
cd /root/repo
git init -b master
git config user.name Test
git config user.email test@example.invalid
git add -A
git commit -qm test
git remote add origin https://github.com/jacobeatsspam/csi-driver-iscsi-targetd.git
E2E_DISPOSABLE_VM=1 bash test/e2e/local-act-in-vm.sh
GUEST
xorriso -as mkisofs -quiet -output "$work/seed.iso" -volid cidata -joliet -rock "$work/seed"
qemu-img create -q -f qcow2 -F qcow2 -b "$(realpath "$image")" "$work/system.qcow2"
qemu-img resize -q "$work/system.qcow2" 24G
set +e
timeout --kill-after=5s 45m qemu-system-x86_64 -enable-kvm -m 6144 -smp 4 -nographic -no-reboot \
	-drive "file=$work/system.qcow2,if=virtio,format=qcow2" \
	-drive "file=$work/seed.iso,media=cdrom,readonly=on" \
	-netdev user,id=net -device virtio-net-pci,netdev=net >"$work/console" 2>&1 &
vm_pid=$!
tail --pid="$vm_pid" -n +1 -f "$work/console" | sed -u -n '/LOCAL_ACT_BEGIN/,$p' &
log_pid=$!
wait "$vm_pid"
status=$?
vm_pid=
set -e
wait "$log_pid" 2>/dev/null || true
log_pid=
if [ "$status" -ne 0 ] || ! tr -d '\r' <"$work/console" | grep -x LOCAL_ACT_PASS >/dev/null; then
	tail -n 40 "$work/console"
	echo "local act VM failed (QEMU status $status)" >&2
	exit 1
fi
