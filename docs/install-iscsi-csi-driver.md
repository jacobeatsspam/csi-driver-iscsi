## Install ISCSI CSI driver on a Kubernetes cluster

- Nodes need `iscsiadm`, `iscsid`, udev, and the iSCSI TCP kernel transport.
- The driver uses the node's iSCSI tools and daemon.
- [install CSI driver master version](./install-csi-driver-master.md)
- [Configure dynamic targetd volumes](./targetd.md)

After unpublish, targetd volumes keep the host iSCSI session and the open-iscsi
node database entry. The driver removes its per-volume connection record.
If no other workload uses the session, an administrator can log out with `iscsiadm`.
Static volumes log out portals that no other driver volume is using.
