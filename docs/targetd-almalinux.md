# Native targetd on AlmaLinux 10.3

A disposable AlmaLinux 10.3 x86_64 KVM guest passed the targetd appliance
test with verified HTTPS, a linear LVM volume group and enforcing SELinux.
The native service ran in `targetd_t`. This test used a local source rebuild
and a local SELinux module.

| Component | Tested version |
| --- | --- |
| targetd | `0.10.4-11.el10` |
| python3-rtslib | `2.1.76-12.el10` |
| libblockdev-lvm | `3.4.0-3.el10.alma.1` |
| selinux-policy-targeted | `42.1.27-2.el10_3.2` |

## Build the native package

Run these commands as root in a fresh AlmaLinux 10.3 test VM.
The Fedora package includes the upstream rtslib compatibility fix.
Its source RPM rebuilt without changes. [Fedora package history](https://packages.fedoraproject.org/pkgs/targetd/targetd/)

```sh
dnf install -y epel-release dnf-plugins-core
dnf config-manager --set-enabled crb
dnf install -y rpm-build pyproject-rpm-macros systemd-rpm-macros \
  python3-devel python3-wheel python3-setuptools python3-gobject-base \
  python3-blockdev libblockdev-lvm python3-rtslib python3-setproctitle \
  policycoreutils-python-utils iscsi-initiator-utils lvm2

curl -fLO https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Everything/source/tree/Packages/t/targetd-0.10.4-11.fc44.src.rpm
curl -fLO https://src.fedoraproject.org/rpms/fedora-repos/raw/f44/f/RPM-GPG-KEY-fedora-44-primary
echo 'bd3654e4470b38fa53f6d22a3b7492943e5fb77614fdaaf49975d71a5be98fc7  targetd-0.10.4-11.fc44.src.rpm' | sha256sum -c -
gpg --show-keys --with-fingerprint RPM-GPG-KEY-fedora-44-primary
```

Before importing the key, match its fingerprint to
`36F6 12DC F27F 7D1A 48A8 35E4 DBFC F71C 6D9F 90A6`. [Fedora signing keys](https://fedoraproject.org/security/)

```sh
rpm --import RPM-GPG-KEY-fedora-44-primary
rpmkeys --checksig -v targetd-0.10.4-11.fc44.src.rpm
rpmbuild --rebuild targetd-0.10.4-11.fc44.src.rpm
dnf install -y /root/rpmbuild/RPMS/noarch/targetd-0.10.4-11.el10.noarch.rpm
```

## Configure the lock file before starting targetd

The stock policy allowed LV creation. It denied `export_create` when rtslib
opened `/var/run/rtslib_backstore.lock`. On AlmaLinux, `/var/run` points
to `/run`. The local policy gives the boot-time file creator and targetd
access to one dedicated file type.

```sh
cat >targetd_rtslib_lock.cil <<'POLICY'
(type targetd_rtslib_lock_t)
(typeattributeset file_type (targetd_rtslib_lock_t))
(allow targetd_t targetd_rtslib_lock_t (file (getattr open read write lock)))
(allow systemd_tmpfiles_t targetd_rtslib_lock_t (file (create getattr open write)))
POLICY
semodule -i targetd_rtslib_lock.cil
semanage fcontext -a -t targetd_rtslib_lock_t '/run/rtslib_backstore\.lock'
printf '%s\n' 'f /run/rtslib_backstore.lock 0600 root root -' >/etc/tmpfiles.d/targetd-rtslib.conf
systemd-tmpfiles --create /etc/tmpfiles.d/targetd-rtslib.conf
restorecon -v /run/rtslib_backstore.lock
stat -c '%U:%G %a %C' /run/rtslib_backstore.lock
```

The expected result is `root:root 600 system_u:object_r:targetd_rtslib_lock_t:s0`.
The guest produced this result after a reboot, before the target and initiator
services started.

## Run the appliance test

The checker creates volumes, exports them, writes data and deletes the test
volumes. Use a disposable VM with an empty scratch VG, an isolated network
and no existing iSCSI sessions. From the driver checkout on a Linux x86_64 host, build the checker and
copy it to `/root/targetd-fixture-check` in the VM.

```sh
CGO_ENABLED=0 go build -mod=vendor -o targetd-fixture-check ./test/e2e/cmd/targetd-fixture-check
```

In the VM, create a loop device backed by a new scratch file.

```sh
modprobe iscsi_target_mod
modprobe iscsi_tcp
modprobe loop
scratch=$(mktemp /var/tmp/targetd-scratch.XXXXXXXX)
truncate -s 1G "$scratch"
loop_device=$(losetup --find --show "$scratch")
lvmdevices --adddev "$loop_device"
pvcreate "$loop_device"
vgcreate vg-targetd-e2e "$loop_device"
```

Configure the fixture identity and a certificate for the VM's loopback address.
The fixture uses fixed test credentials. Keep the VM's management and iSCSI
ports private.

```sh
cat >/etc/target/targetd.yaml <<'YAML'
user: e2e
password: fixture-only
ssl: true
block_pools: [vg-targetd-e2e]
fs_pools: []
zfs_block_pools: []
target_name: iqn.2026-01.test.csi:targetd
portal_addresses: [127.0.0.1]
YAML
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=targetd-lock-test \
  -addext subjectAltName=IP:127.0.0.1 \
  -keyout /etc/target/targetd_key.pem -out /etc/target/targetd_cert.pem
chmod 0600 /etc/target/targetd.yaml /etc/target/targetd_{key,cert}.pem
restorecon /etc/target/targetd{.yaml,_key.pem,_cert.pem}
echo 'InitiatorName=iqn.2026-01.test.csi:initiator' >/etc/iscsi/initiatorname.iscsi
systemctl start targetd iscsid
getenforce
ps -Z -p "$(systemctl show -p MainPID --value targetd)"
SSL_CERT_FILE=/etc/target/targetd_cert.pem TARGETD_E2E_TLS=1 \
  TARGETD_E2E_DISPOSABLE_VM=1 /root/targetd-fixture-check
```

After the test, shut down and discard the scratch VM.

## Test results

The checker created a 64 MiB linear LV. The published device reported the LV
UUID in SCSI VPD page 0x80. A write through iSCSI reached the backing LV.
The export and data survived a native targetd service restart. Logout, unexport
and deletion left an empty VG. The production Go client also read the pool
inventory over verified HTTPS.

A separate test retained an exported LV across a full VM reboot. After the
scratch VG came online, the native target service restored the export. A fresh
login found the same LUN, serial and data. The test then removed the export
and LV.

The successful HTTPS run kept SELinux enforcing. The audit recorded a denied
terminal query (`ioctl 0x5401`) on the lock and a denied search of a
`targetd_var_t` directory. Neither operation required a policy grant for this
lifecycle.
