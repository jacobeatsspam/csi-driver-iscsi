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

cluster=${E2E_KIND_CLUSTER:-kind}
if [[ ${E2E_DISPOSABLE_VM:-} != 1 ]] || ! systemd-detect-virt --vm --quiet || [ "$#" -ne 1 ] || [ ! -d "$1" ] || [ "$(kubectl config current-context)" != "kind-$cluster" ]; then
  echo 'CSI fixture requires a work directory and the selected kind cluster inside a disposable VM.' >&2
  exit 2
fi
diagnostics() {
  if [ "$?" -ne 0 ]; then
    kubectl -n kube-system get pods -l 'app in (csi-iscsi-controller,csi-iscsi-node)' -o wide >&2 || true
    kubectl -n kube-system logs deployment/csi-iscsi-controller -c iscsi --tail=100 >&2 || true
    kubectl -n kube-system logs deployment/csi-iscsi-controller -c attacher --tail=100 >&2 || true
    kubectl -n kube-system logs daemonset/csi-iscsi-node -c iscsi --tail=100 >&2 || true
    kubectl -n kube-system get configmap csi-iscsi-targetd-barrier -o yaml >&2 || true
    kubectl -n targetd-e2e get pods,pvc >&2 || true
    kubectl get pv,volumeattachment -o yaml >&2 || true
    kubectl -n kube-system get events --sort-by=.lastTimestamp >&2 || true
    kubectl -n targetd-e2e get events --sort-by=.lastTimestamp >&2 || true
    if declare -F device_report >/dev/null && [ -n "${device:-}" ]; then device_report >&2 || true; fi
  fi
}
trap diagnostics EXIT
image=iscsi-driver:targetd-e2e
node="$cluster-control-plane"
gateway=$(docker inspect "$node" --format '{{(index .NetworkSettings.Networks "kind").Gateway}}')
test -n "$gateway"
device_report() {
  docker exec "$node" sh -c '
    path=$1
    echo "Fixture iSCSI device: $path"
    lsblk -dn -o NAME,SERIAL,WWN "$path" || true
    SYSTEMD_PAGER=cat udevadm info -q property -n "$path" | grep -E "^(ID_SCSI_SERIAL|ID_SERIAL|SCSI_IDENT_SERIAL)=" || true
    resolved=$(readlink -f "$path")
    name=${resolved##*/}
    od -An -tx1 -v "/sys/class/block/$name/device/vpd_pg80" || true
  ' sh "$device"
}
logout_fixture() {
  local sessions
  sessions=$(docker exec "$node" sh -c 'for f in /sys/class/iscsi_session/session*/targetname; do [ -f "$f" ] || continue; [ "$(cat "$f")" != "iqn.2026-01.test.csi:targetd" ] || echo found; done')
  if [ -n "$sessions" ]; then
    docker exec "$node" iscsiadm -m node -T iqn.2026-01.test.csi:targetd -p "$gateway:3260" --logout
  fi
}

wait_unpublished() {
  local volume=$1 handle=$2 mounts
  for ((i=0; i<120; i++)); do
    mounts=$(docker exec "$node" cat /proc/1/mountinfo)
    if ! grep -Fq "/$volume/" <<<"$mounts" &&
       docker exec "$node" sh -c 'test -d /var/lib/iscsi.csi.k8s.io && test ! -e "/var/lib/iscsi.csi.k8s.io/iscsi-$1.json"' sh "$handle"; then
      return 0
    fi
    sleep 1
  done
  echo "Test volume $volume remains mounted or its connector record is not gone; refusing fixture logout." >&2
  return 1
}

docker build --build-arg ARCH=amd64 -t "$image" .
kind load docker-image --name "$cluster" "$image"
docker exec "$node" bash -c 'printf "%s\n" "ACTION==\"add\", SUBSYSTEM==\"block\", ENV{ID_PATH}==\"*iscsi-iqn.2026-01.test.csi:targetd-*\", RUN+=\"/usr/bin/mknod /dev/%k b %M %m\"" "ACTION==\"remove\", SUBSYSTEM==\"block\", ENV{ID_PATH}==\"*iscsi-iqn.2026-01.test.csi:targetd-*\", RUN+=\"/usr/bin/rm -f /dev/%k\"" > /etc/udev/rules.d/99-targetd-e2e.rules; udevadm control --reload'
cat > "$1/config.json" <<CONFIG
{"backendID":"targetd-e2e","endpoint":"https://$gateway:18700/targetrpc","pools":["vg-targetd-e2e"],"namespace":"targetd-e2e","targetIQN":"iqn.2026-01.test.csi:targetd","portals":["$gateway:3260"],"username":"e2e","passwordFile":"/etc/targetd/credentials/password","caFile":"/etc/targetd/ca/ca.crt"}
CONFIG
kubectl -n kube-system create configmap csi-targetd-config --from-file=config.json="$1/config.json"
kubectl -n kube-system create configmap csi-targetd-ca --from-file=ca.crt="$1/target/targetd_cert.pem"
kubectl -n kube-system create secret generic csi-targetd-credentials --from-literal=password=fixture-only
for manifest in deploy/targetd/csi-iscsi-driverinfo.yaml deploy/targetd/csi-iscsi-controller.yaml deploy/targetd/csi-iscsi-node.yaml; do
  sed "s|gcr.io/k8s-staging-sig-storage/iscsiplugin:canary|$image|g" "$manifest" | kubectl create -f -
done
kubectl -n kube-system rollout status deployment/csi-iscsi-controller --timeout=180s
kubectl -n kube-system rollout status daemonset/csi-iscsi-node --timeout=180s
kubectl create namespace targetd-e2e
cat > "$1/workload.yaml" <<'YAML'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: targetd-e2e
provisioner: iscsi.csi.k8s.io
parameters:
  pool: vg-targetd-e2e
reclaimPolicy: Delete
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: targetd-e2e
  namespace: targetd-e2e
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: targetd-e2e
  resources:
    requests:
      storage: 64Mi
YAML
cat > "$1/pod.yaml" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: targetd-e2e
  namespace: targetd-e2e
spec:
  containers:
    - name: test
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: volume
          mountPath: /mnt/volume
  volumes:
    - name: volume
      persistentVolumeClaim:
        claimName: targetd-e2e
YAML
kubectl create -f "$1/workload.yaml"
kubectl -n targetd-e2e wait --for=jsonpath='{.status.phase}'=Bound pvc/targetd-e2e --timeout=180s
bash test/e2e/targetd/controller-handoff.sh "$1"
pv=$(kubectl -n targetd-e2e get pvc targetd-e2e -o jsonpath='{.spec.volumeName}')
cat > "$1/attachment.yaml" <<YAML
apiVersion: storage.k8s.io/v1
kind: VolumeAttachment
metadata:
  name: targetd-e2e-negative
spec:
  attacher: iscsi.csi.k8s.io
  nodeName: $node
  source:
    persistentVolumeName: $pv
YAML
kubectl create -f "$1/attachment.yaml"
kubectl wait --for=jsonpath='{.status.attached}'=true volumeattachment/targetd-e2e-negative --timeout=180s
kubectl get "pv/$pv" -o json > "$1/pv.json"
kubectl get volumeattachment/targetd-e2e-negative -o json > "$1/attachment.json"
CGO_ENABLED=0 go build -mod=vendor -o "$1/negative-publish" ./test/e2e/cmd/negative-publish
docker exec "$node" mkdir -p /usr/local/bin
docker exec -i "$node" sh -c 'cat > /usr/local/bin/targetd-negative-publish && chmod 0755 /usr/local/bin/targetd-negative-publish' < "$1/negative-publish"
docker exec -i "$node" sh -c 'cat > /usr/local/bin/targetd-negative-pv.json' < "$1/pv.json"
docker exec -i "$node" sh -c 'cat > /usr/local/bin/targetd-negative-attachment.json' < "$1/attachment.json"
docker exec "$node" sh -c 'test -x /usr/local/bin/targetd-negative-publish && test -s /usr/local/bin/targetd-negative-pv.json && test -s /usr/local/bin/targetd-negative-attachment.json'
docker exec "$node" /usr/local/bin/targetd-negative-publish /usr/local/bin/targetd-negative-pv.json /usr/local/bin/targetd-negative-attachment.json
docker exec "$node" rm -f /usr/local/bin/targetd-negative-publish /usr/local/bin/targetd-negative-pv.json /usr/local/bin/targetd-negative-attachment.json
lun=$(kubectl get volumeattachment/targetd-e2e-negative -o jsonpath='{.status.attachmentMetadata.lun}')
device="/dev/disk/by-path/ip-$gateway:3260-iscsi-iqn.2026-01.test.csi:targetd-lun-$lun"
docker exec "$node" test -b "$device"
echo "Expected targetd volume UUID: $(kubectl get volumeattachment/targetd-e2e-negative -o jsonpath='{.status.attachmentMetadata.volumeUUID}')"
device_report
set +e
docker exec "$node" blkid -p "$device"
status=$?
set -e
if [ "$status" -ne 2 ] || docker exec "$node" test -e /var/lib/kubelet/plugins/iscsi.csi.k8s.io/targetd-negative; then
  echo 'Wrong-serial publish changed or mounted the test disk.' >&2
  exit 1
fi
logout_fixture

# The manual VolumeAttachment supplies an existing export for a static PV.
cat > "$1/static.yaml" <<YAML
apiVersion: v1
kind: PersistentVolume
metadata:
  name: targetd-static
spec:
  capacity: {storage: 64Mi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  csi:
    driver: iscsi.csi.k8s.io
    volumeHandle: targetd-static
    fsType: ext4
    volumeAttributes:
      targetPortal: "$gateway:3260"
      iqn: iqn.2026-01.test.csi:targetd
      lun: "$lun"
      discovery: "true"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: targetd-static
  namespace: targetd-e2e
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: targetd-static
  resources:
    requests: {storage: 64Mi}
YAML
kubectl create -f "$1/static.yaml"
sed 's/claimName: targetd-e2e/claimName: targetd-static/' "$1/pod.yaml" | kubectl create -f -
kubectl -n targetd-e2e wait --for=condition=Ready pod/targetd-e2e --timeout=180s
kubectl -n targetd-e2e exec targetd-e2e -- sh -ec 'echo static-volume > /mnt/volume/static; sync'
docker exec "$node" test -s /var/lib/iscsi.csi.k8s.io/iscsi-targetd-static.json
kubectl -n targetd-e2e delete pod targetd-e2e --wait=true --timeout=180s
wait_unpublished targetd-static targetd-static
kubectl delete -f "$1/static.yaml" --wait=true --timeout=180s
kubectl delete -f "$1/attachment.yaml" --wait=true --timeout=180s
kubectl create -f "$1/pod.yaml"
kubectl -n targetd-e2e wait --for=condition=Ready pod/targetd-e2e --timeout=180s
kubectl -n targetd-e2e exec targetd-e2e -- sh -ec 'test "$(cat /mnt/volume/static)" = static-volume; echo targetd-e2e-data > /mnt/volume/data; sync'
kubectl -n kube-system rollout restart deployment/csi-iscsi-controller daemonset/csi-iscsi-node
kubectl -n kube-system rollout status deployment/csi-iscsi-controller --timeout=180s
kubectl -n kube-system rollout status daemonset/csi-iscsi-node --timeout=180s
handle=$(kubectl get "pv/$pv" -o jsonpath='{.spec.csi.volumeHandle}')
docker exec "$node" test -s "/var/lib/iscsi.csi.k8s.io/iscsi-$handle.json"
kubectl -n targetd-e2e delete pod targetd-e2e --wait=true --timeout=180s
wait_unpublished "$pv" "$handle"
kubectl create -f "$1/pod.yaml"
kubectl -n targetd-e2e wait --for=condition=Ready pod/targetd-e2e --timeout=180s
kubectl -n targetd-e2e exec targetd-e2e -- sh -c 'test "$(cat /mnt/volume/data)" = targetd-e2e-data'

# Keep the first LUN mounted while provisioning and publishing a second LUN to
# the same node and target. Removing the first must not disturb the second.
cat > "$1/second.yaml" <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: targetd-e2e-second
  namespace: targetd-e2e
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: targetd-e2e
  resources:
    requests:
      storage: 64Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: targetd-e2e-second
  namespace: targetd-e2e
spec:
  containers:
    - name: test
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: volume
          mountPath: /mnt/volume
  volumes:
    - name: volume
      persistentVolumeClaim:
        claimName: targetd-e2e-second
YAML
kubectl create -f "$1/second.yaml"
kubectl -n targetd-e2e wait --for=jsonpath='{.status.phase}'=Bound pvc/targetd-e2e-second --timeout=180s
pv_second=$(kubectl -n targetd-e2e get pvc targetd-e2e-second -o jsonpath='{.spec.volumeName}')
kubectl -n targetd-e2e wait --for=condition=Ready pod/targetd-e2e-second --timeout=180s
kubectl -n targetd-e2e exec targetd-e2e-second -- sh -ec 'echo second-volume-data > /mnt/volume/data; sync'
kubectl -n targetd-e2e exec targetd-e2e -- sh -c 'test "$(cat /mnt/volume/data)" = targetd-e2e-data'

# CSI IDs contain the targetd LV name in fields four through seven.
handle_second=$(kubectl get "pv/$pv_second" -o jsonpath='{.spec.csi.volumeHandle}')
docker exec "$node" test -s "/var/lib/iscsi.csi.k8s.io/iscsi-$handle_second.json"
lv=$(cut -d_ -f4-7 <<<"$handle")
lv_second=$(cut -d_ -f4-7 <<<"$handle_second")
if [ -z "$lv" ] || [ -z "$lv_second" ] || [ "$lv" = "$lv_second" ]; then
  echo 'Expected two distinct targetd LV names.' >&2
  exit 1
fi
lvs --noheadings -o lv_name "vg-targetd-e2e/$lv" "vg-targetd-e2e/$lv_second" >/dev/null


kubectl -n targetd-e2e delete pod targetd-e2e --wait=true --timeout=180s
wait_unpublished "$pv" "$handle"
kubectl -n targetd-e2e delete pvc targetd-e2e --wait=false
kubectl wait --for=delete "pv/$pv" --timeout=180s
remaining_lvs=$(lvs --noheadings -o lv_name vg-targetd-e2e)
if grep -Fxq "$lv" <<<"$(awk '{$1=$1; print}' <<<"$remaining_lvs")" ||
   ! grep -Fxq "$lv_second" <<<"$(awk '{$1=$1; print}' <<<"$remaining_lvs")"; then
  echo 'Removing the first PVC did not preserve only the second LV.' >&2
  exit 1
fi
kubectl -n targetd-e2e exec targetd-e2e-second -- sh -ec 'test "$(cat /mnt/volume/data)" = second-volume-data; echo still-usable >> /mnt/volume/data; sync; test "$(tail -n 1 /mnt/volume/data)" = still-usable'
kubectl -n targetd-e2e delete pod targetd-e2e-second --wait=true --timeout=180s
wait_unpublished "$pv_second" "$handle_second"
logout_fixture
kubectl patch "pv/$pv_second" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
kubectl -n targetd-e2e delete pvc targetd-e2e-second --wait=true --timeout=180s
kubectl wait --for=jsonpath='{.status.phase}'=Released "pv/$pv_second" --timeout=180s
lvs --noheadings -o lv_name "vg-targetd-e2e/$lv_second" >/dev/null
kubectl patch "pv/$pv_second" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl wait --for=delete "pv/$pv_second" --timeout=180s
remaining_lvs=$(lvs --noheadings -o lv_name vg-targetd-e2e)
if [[ $remaining_lvs =~ [^[:space:]] ]]; then
  echo 'A test volume still exists after PVC deletion.' >&2
  exit 1
fi
kubectl delete -f "$1/second.yaml" --ignore-not-found
kubectl delete -f "$1/workload.yaml" --ignore-not-found
kubectl delete namespace targetd-e2e --wait=true --timeout=180s
for manifest in deploy/targetd/csi-iscsi-node.yaml deploy/targetd/csi-iscsi-controller.yaml deploy/targetd/csi-iscsi-driverinfo.yaml; do
  kubectl delete -f "$manifest" --ignore-not-found --wait=true --timeout=180s
done
kubectl -n kube-system delete configmap csi-targetd-config csi-targetd-ca
kubectl -n kube-system delete secret csi-targetd-credentials
echo 'Static and dynamic PVCs, shared-target isolation, driver restart, remount, Retain and Delete passed.'
