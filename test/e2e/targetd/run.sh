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
if [[ ${E2E_DISPOSABLE_VM:-} != 1 ]] || ! systemd-detect-virt --vm --quiet; then
  echo 'Targetd tests require E2E_DISPOSABLE_VM=1 inside a disposable VM.' >&2
  exit 2
fi
cluster=${E2E_KIND_CLUSTER:-kind}
if [ "$(kubectl config current-context)" != "kind-$cluster" ] || ! docker inspect "$cluster-control-plane" >/dev/null 2>&1; then
  echo 'Targetd tests require the selected disposable kind cluster as the current context.' >&2
  exit 2
fi
actual_server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kind_server=$(kind get kubeconfig --name "$cluster" | kubectl config view --kubeconfig=/dev/stdin --minify -o jsonpath='{.clusters[0].cluster.server}')
if [ -z "$actual_server" ] || [ "$actual_server" != "$kind_server" ]; then
  echo 'Current Kubernetes server does not match the selected kind cluster.' >&2
  exit 2
fi
if [ "$(id -u)" != 0 ]; then
  bin=$(mktemp)
  trap 'rm -f "$bin"' EXIT
  go build -mod=vendor -o "$bin" ./test/e2e/cmd/targetd-fixture-check
  sudo env E2E_DISPOSABLE_VM=1 TARGETD_CHECK_BINARY="$bin" PATH="$PATH" \
    KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" E2E_KIND_CLUSTER="${E2E_KIND_CLUSTER:-kind}" bash "$0"
  exit
fi
root=$(cd "$(dirname "$0")/../../.." && pwd)
if [[ ${TARGETD_E2E_LOCKED:-} != 1 ]]; then
  exec env TARGETD_E2E_LOCKED=1 bash "$root/test/e2e/with-iscsid-isolation.sh" --lock-only bash "$0"
fi
if ! [ "/proc/$$/fd/9" -ef /run/lock/csi-iscsi-e2e/iscsid.lock ] || ! flock -n 9; then
  echo 'Targetd fixture must hold the host iSCSI test lock.' >&2
  exit 1
fi
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$root/test/e2e/targetd" -p '*_test.py'
work=$(mktemp -d /tmp/targetd-e2e.XXXXXXXX)
loop=
kind_stopped=0
initiator_changed=0
initiator_existed=0
restore_initiator() {
  if [ "$initiator_changed" != 1 ]; then return 0; fi
  if [ "$initiator_existed" = 1 ]; then
    local restore_dir
    restore_dir=$(mktemp -d /etc/iscsi/.csi-init.XXXXXXXX) || return 1
    if ! cp -a "$work/initiatorname.iscsi" "$restore_dir/initiator" ||
       ! mv -Tf "$restore_dir/initiator" /etc/iscsi/initiatorname.iscsi; then
      rm -rf "$restore_dir"
      return 1
    fi
    rmdir "$restore_dir" || return 1
  else
    rm -f /etc/iscsi/initiatorname.iscsi || return 1
  fi
  initiator_changed=0
}
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    if [ "$initiator_changed" = 1 ]; then
      if [ "$(systemctl show iscsid.service --property=ActiveState --value)" = inactive ] && sessions_empty; then
        restore_initiator || echo "Could not restore the original initiator file from $work." >&2
      else
        echo "Initiator configuration retained in $work; restore it after stopping fixture sessions and iscsid." >&2
      fi
    fi
    if [ "$kind_stopped" = 1 ] && [ "$(docker inspect --format '{{.State.Running}}' "$kind_daemon" 2>/dev/null)" != true ]; then
      if [ "$(systemctl show iscsid.service --property=ActiveState --value)" = inactive ] && sessions_empty; then
        E2E_KIND_CLUSTER="$cluster" bash "$root/test/e2e/targetd/kind-daemon.sh" start || echo "Could not safely restart fixture daemon $kind_daemon." >&2
      else
        echo "Fixture daemon $kind_daemon remains stopped. Restart it only after stopping host iscsid and clearing all iSCSI sessions." >&2
      fi
    fi
    docker logs targetd-e2e >&2 2>/dev/null || true
    docker top targetd-e2e -eo pid,stat,args >&2 2>/dev/null || true
    ls -l /dev/mapper /dev/vg-targetd-e2e >&2 2>/dev/null || true
    dmsetup ls >&2 2>/dev/null || true
    echo "Fixture state retained in $work for inspection." >&2
  fi
}
trap cleanup EXIT
sessions_empty() {
  [ -d /sys/class/iscsi_session ] || return 1
  local sessions
  sessions=$(ls -A /sys/class/iscsi_session) || return 1
  [ -z "$sessions" ]
}
if ! sessions_empty || [ "$(systemctl show iscsid.service --property=ActiveState --value)" != inactive ]; then
  echo 'Host iSCSI service or session state is active or unobservable; refusing the fixture.' >&2
  exit 1
fi
iscsid_state=$(systemctl is-enabled iscsid.service 2>/dev/null || true)
if [[ $iscsid_state == masked || $iscsid_state == masked-runtime ]]; then
  echo 'Host iscsid is masked; refusing to change its existing mask.' >&2
  exit 1
fi
kind_daemon="${cluster}-iscsid"
bash "$root/test/e2e/targetd/kind-daemon.sh" check
owner=$(docker inspect --format '{{ index .Config.Labels "iscsi.csi.k8s.io/e2e-kind-iscsid" }}' "$kind_daemon")
if [ "$owner" != "$cluster" ] || [ "$(docker inspect --format '{{.State.Running}}' "$kind_daemon")" != true ]; then
  echo 'The fixture-owned kind iscsid container is missing or not running.' >&2
  exit 1
fi
docker stop "$kind_daemon" >/dev/null
kind_stopped=1
if [ -e /etc/iscsi/initiatorname.iscsi ] || [ -L /etc/iscsi/initiatorname.iscsi ]; then
  cp -a /etc/iscsi/initiatorname.iscsi "$work/initiatorname.iscsi"
  initiator_existed=1
fi
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y lvm2 open-iscsi
existing_vg=$(vgs --noheadings -o vg_name --select vg_name=vg-targetd-e2e)
if docker container inspect targetd-e2e >/dev/null 2>&1 || [ -n "$existing_vg" ]; then
  echo 'Targetd test resources already exist; refusing to overwrite them.' >&2
  exit 1
fi
modprobe iscsi_target_mod
modprobe iscsi_tcp
if ! mountpoint -q /sys/kernel/config || [ ! -d /sys/kernel/config/target ] ||
   [ -e /sys/kernel/config/target/iscsi/iqn.2026-01.test.csi:targetd ]; then
  echo 'Cannot establish that the fixture target IQN is unused in configfs.' >&2
  exit 1
fi
if ! sessions_empty; then
  echo 'iSCSI sessions exist or cannot be observed; refusing to start the targetd fixture.' >&2
  exit 1
fi
if [ "$(systemctl show iscsid.service --property=ActiveState --value)" != inactive ]; then
  echo 'Host iscsid is already active; refusing to manage it.' >&2
  exit 1
fi
iscsid_state=$(systemctl is-enabled iscsid.service 2>/dev/null || true)
if [[ $iscsid_state == masked || $iscsid_state == masked-runtime ]]; then
  echo 'Host iscsid is masked; refusing to change its existing mask.' >&2
  exit 1
fi
truncate -s 1G "$work/disk"
loop=$(losetup --find --show "$work/disk")
pvcreate "$loop"
vgcreate vg-targetd-e2e "$loop"
mkdir -p "$work/target"
gateway=$(docker inspect "$cluster-control-plane" --format '{{(index .NetworkSettings.Networks "kind").Gateway}}')
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=targetd-e2e \
  -addext "subjectAltName=IP:127.0.0.1,IP:$gateway" \
  -keyout "$work/target/targetd_key.pem" -out "$work/target/targetd_cert.pem"
chmod 600 "$work/target/"*.pem
cat > "$work/target/targetd.yaml" <<'CONFIG'
user: e2e
password: fixture-only
ssl: true
block_pools: [vg-targetd-e2e]
fs_pools: []
zfs_block_pools: []
target_name: iqn.2026-01.test.csi:targetd
portal_addresses: [0.0.0.0]
CONFIG
chmod 600 "$work/target/targetd.yaml"
initiator_changed=1
rm -f /etc/iscsi/initiatorname.iscsi
printf '%s\n' 'InitiatorName=iqn.2026-01.test.csi:initiator' > /etc/iscsi/initiatorname.iscsi
systemctl start iscsid
cd "$root"
docker build -t targetd-e2e -f test/e2e/targetd/Dockerfile test/e2e/targetd
docker run -d --name targetd-e2e --privileged --network host \
  -v /dev:/dev -v /sys/kernel/config:/sys/kernel/config \
  -v /run/lvm:/run/lvm -v /run/lock/lvm:/run/lock/lvm \
  -v "$work/target:/etc/target" targetd-e2e
if [ -z "${TARGETD_CHECK_BINARY:-}" ]; then
  go build -mod=vendor -o "$work/targetd-fixture-check" ./test/e2e/cmd/targetd-fixture-check
  TARGETD_CHECK_BINARY=$work/targetd-fixture-check
fi
SSL_CERT_FILE="$work/target/targetd_cert.pem" TARGETD_E2E_TLS=1 TARGETD_E2E_DISPOSABLE_VM=1 TARGETD_E2E_CONTAINER=1 "$TARGETD_CHECK_BINARY"
if ! sessions_empty; then
  echo 'Host-side fixture sessions remain or cannot be observed; retaining the host daemon.' >&2
  exit 1
fi
# The host-side fixture started this service. The kind phase has its own daemon.
systemctl stop iscsid.service
restore_initiator
E2E_DRIVER_DELIVERY=kind bash test/e2e/with-iscsid-isolation.sh bash -c 'bash "$1" start && exec bash "$2" "$3"' _ test/e2e/targetd/kind-daemon.sh test/e2e/targetd/csi-test.sh "$work"
kind_stopped=0
for ((i=0; i<30; i++)); do
  if sessions_empty; then break; fi
  sleep 1
done
if ! sessions_empty; then
  echo 'iSCSI sessions remain or cannot be observed; refusing to remove the fixture target.' >&2
  exit 1
fi
docker exec -i targetd-e2e python3 - < test/e2e/targetd/cleanup_target.py
docker rm -f targetd-e2e
vgremove -y vg-targetd-e2e
pvremove -y "$loop"
losetup -d "$loop"
rm -rf "$work"
