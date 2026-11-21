/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package iscsi

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	iscsiLib "github.com/kubernetes-csi/csi-driver-iscsi/pkg/iscsilib"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	klog "k8s.io/klog/v2"

	"k8s.io/utils/mount"
)

type ISCSIUtil struct{}

var iscsiOperations sync.Mutex

func (util *ISCSIUtil) AttachDisk(b iscsiDiskMounter) (string, error) {
	iscsiOperations.Lock()
	defer iscsiOperations.Unlock()
	if b.connector == nil {
		return "", fmt.Errorf("connector is nil")
	}

	iscsiInfoPath := getIscsiInfoPath(b.VolName)
	data, err := os.ReadFile(iscsiInfoPath)
	if err == nil {
		var previous iscsiLib.Connector
		if json.Unmarshal(data, &previous) != nil || !sameConnectorTarget(&previous, b.connector) {
			return "", fmt.Errorf("volume ID already has a different iSCSI connection")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	devicePath, err := (*b.connector).Connect()
	if err != nil {
		return "", err
	}
	if devicePath == "" {
		return "", fmt.Errorf("connect reported success, but no path returned")
	}
	if b.volumeUUID != "" {
		if err := verifyVolumeSerial(b.connector.Devices, b.volumeUUID, hostDeviceSerial); err != nil {
			return "", err
		}
	}
	// Mount device
	mntPath := b.targetPath
	notMnt, err := mount.IsNotMountPoint(b.mounter, mntPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect target mount: %w", err)
	}
	if err := os.MkdirAll(mntPath, 0o750); err != nil {
		klog.Errorf("iscsi: failed to mkdir %s, error", mntPath)
		return "", err
	}

	options, readOnly := b.effectiveMountOptions()
	if !notMnt {
		target, err := mountedTarget(mntPath, devicePath)
		if err != nil {
			return "", err
		}
		if target.Type != b.fsType && b.fsType != "" || slices.Contains(target.Opts, "ro") != readOnly {
			return "", status.Error(codes.AlreadyExists, "target is already mounted with a different filesystem or access mode")
		}
	}

	if b.volumeUUID == "" {
		recordDeviceWWID(b.connector)
	}

	// Persist iscsi disk config to json file for DetachDisk path
	err = iscsiLib.PersistConnector(b.connector, iscsiInfoPath)
	if err != nil {
		klog.Errorf("failed to persist connection info: %v", err)
		return "", fmt.Errorf("unable to create persistence file for connection")
	}

	if !notMnt {
		klog.Infof("iscsi: %s already mounted", mntPath)
		return devicePath, nil
	}

	err = b.mounter.FormatAndMount(devicePath, mntPath, b.fsType, options)
	if err != nil {
		klog.Errorf("iscsi: failed to mount iscsi volume %s [%s] to %s, error %v", devicePath, b.fsType, mntPath, err)
	}

	return devicePath, err
}

func (b iscsiDiskMounter) effectiveMountOptions() ([]string, bool) {
	options := make([]string, 0, len(b.mountOptions)+1)
	readOnly := false
	for _, flag := range b.mountOptions {
		for _, option := range strings.Split(flag, ",") {
			switch option {
			case "ro":
				readOnly = true
			case "rw":
				readOnly = false
			default:
				options = append(options, option)
			}
		}
	}
	if readOnly || b.readOnly {
		return append(options, "ro"), true
	}
	return append(options, "rw"), false
}

func recordDeviceWWID(connector *iscsiLib.Connector) {
	connector.DeviceWWID = ""
	if connector.MountTargetDevice == nil || connector.MountTargetDevice.Type != "disk" || len(connector.Devices) != 1 {
		return
	}
	wwid, err := connector.MountTargetDevice.WWID()
	if err != nil {
		klog.Warningf("cannot record iSCSI device WWID: %v", err)
		return
	}
	connector.DeviceWWID = wwid
}

func sameConnectorTarget(a, b *iscsiLib.Connector) bool {
	iface := func(name string) string {
		if name == "" {
			return "default"
		}
		return name
	}
	if a.VolumeName != b.VolumeName || a.TargetIqn != b.TargetIqn || a.Lun != b.Lun || a.VolumeUUID != b.VolumeUUID || iface(a.Interface) != iface(b.Interface) || len(a.TargetPortals) != len(b.TargetPortals) {
		return false
	}
	return slices.Equal(slices.Sorted(slices.Values(a.TargetPortals)), slices.Sorted(slices.Values(b.TargetPortals)))
}

func hostDeviceSerial(device iscsiLib.Device) (string, error) {
	return readDeviceSerial("/sys/class/block", device.Name)
}

func readDeviceSerial(sysBlock, deviceName string) (string, error) {
	if deviceName == "" || deviceName == "." || deviceName == ".." || filepath.Base(deviceName) != deviceName {
		return "", fmt.Errorf("invalid iSCSI device name")
	}
	page, err := os.ReadFile(filepath.Join(sysBlock, deviceName, "device", "vpd_pg80"))
	if err != nil {
		return "", fmt.Errorf("cannot read iSCSI device serial: %w", err)
	}
	if len(page) < 4 || page[1] != 0x80 || int(binary.BigEndian.Uint16(page[2:4])) != len(page)-4 {
		return "", fmt.Errorf("invalid iSCSI device serial page")
	}
	serial := strings.TrimLeft(string(page[4:]), " \t\r\n")
	serial = strings.TrimRight(serial, "\x00 \t\r\n")
	if !safeTargetdID.MatchString(serial) {
		return "", fmt.Errorf("invalid iSCSI device serial")
	}
	return serial, nil
}

func verifyVolumeSerial(devices []iscsiLib.Device, expected string, serial func(iscsiLib.Device) (string, error)) error {
	if expected == "" || len(devices) == 0 {
		return fmt.Errorf("cannot verify dynamic volume identity")
	}
	for _, device := range devices {
		actual, err := serial(device)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("iSCSI device serial does not match volume UUID")
		}
	}
	return nil
}

func (util *ISCSIUtil) DetachDisk(c iscsiDiskUnmounter, targetPath string) error {
	iscsiOperations.Lock()
	defer iscsiOperations.Unlock()
	return detachDisk(c, targetPath, getIscsiInfoPath(c.VolName))
}

func detachDisk(c iscsiDiskUnmounter, targetPath, iscsiInfoPath string) error {
	device, _, err := mount.GetDeviceNameFromMount(c.mounter, targetPath)
	if err != nil {
		klog.Errorf("iscsi detach disk: failed to get device from mnt: %s\nError: %v", targetPath, err)
		return err
	}
	klog.Infof("loading ISCSI connection info from %s", iscsiInfoPath)
	data, err := os.ReadFile(iscsiInfoPath)
	if err != nil {
		if os.IsNotExist(err) {
			if device != "" {
				return fmt.Errorf("cannot safely unpublish mounted volume without its connection record")
			}
			klog.Warningf("assuming that ISCSI connection is already closed")
			return nil
		}
		return status.Error(codes.Internal, err.Error())
	}
	connector := &iscsiLib.Connector{}
	if err := json.Unmarshal(data, connector); err != nil {
		return fmt.Errorf("invalid iSCSI connection record %s: %w", iscsiInfoPath, err)
	}
	if connector.TargetIqn == "" || len(connector.TargetPortals) == 0 {
		return fmt.Errorf("invalid iSCSI connection record %s", iscsiInfoPath)
	}
	if strings.HasPrefix(c.VolName, "td1_") || connector.VolumeUUID != "" {
		parts := strings.Split(c.VolName, "_")
		if len(parts) != 8 || parts[0] != "td1" || parts[3] != "csitd" || !safeTargetdID.MatchString(parts[7]) || connector.VolumeName != c.VolName || connector.VolumeUUID != parts[7] {
			return fmt.Errorf("dynamic volume ID and connection record do not match")
		}
		return detachTargetdDisk(c, targetPath, iscsiInfoPath, device, connector)
	}
	portals, shared, err := unsharedPortals(iscsiInfoPath, connector)
	if err != nil {
		return err
	}
	if !connector.DevicesDetached {
		var inUse bool
		if device == "" {
			inUse, err = recoverDevice(connector, "/dev/disk/by-path", "/sys/class/scsi_device")
			if err != nil {
				return fmt.Errorf("cannot safely resume iSCSI device removal for %s: %w", c.VolName, err)
			}
		} else {
			if err := validateAttachedDevices(connector, "/dev"); err != nil {
				return fmt.Errorf("cannot verify attached iSCSI device: %w", err)
			}
			connector, err = iscsiLib.GetConnectorFromFile(iscsiInfoPath)
			if err != nil {
				return err
			}
			if _, err := mountedTarget(targetPath, connector.MountTargetDevice.GetPath()); err != nil {
				return err
			}
			if err = c.mounter.Unmount(targetPath); err != nil {
				return fmt.Errorf("failed to unmount %s: %w", targetPath, err)
			}
			inUse, err = deviceMounted(c.mounter, connector.MountTargetDevice.GetPath())
			if err != nil {
				return err
			}
		}
		if inUse {
			klog.Infof("the device is still in use: %s", connector.MountTargetDevice.GetPath())
			return nil
		}
		if err := removeVolumeDevices(connector, iscsiInfoPath); err != nil {
			return err
		}
	} else if device != "" {
		return fmt.Errorf("cannot log out: device %s is mounted at %s", device, targetPath)
	}

	for _, portal := range portals {
		if err := iscsiLib.Logout(connector.TargetIqn, portal); err != nil {
			return err
		}
	}
	if !shared {
		if err := iscsiLib.DeleteDBEntry(connector.TargetIqn); err != nil {
			return err
		}
	}
	return finishDetach(targetPath, iscsiInfoPath)
}

func removeVolumeDevices(connector *iscsiLib.Connector, infoPath string) error {
	if !connector.DevicesDetached {
		// A retry must know whether a missing device was removed by an earlier attempt.
		connector.DevicesDetaching = true
		if err := connector.Persist(infoPath); err != nil {
			return err
		}
		if err := connector.DisconnectVolume(); err != nil {
			return fmt.Errorf("failed to disconnect iSCSI volume: %w", err)
		}
		connector.DevicesDetached = true
	}
	connector.DevicesDetaching = false
	return connector.Persist(infoPath)
}

func recoverDevice(connector *iscsiLib.Connector, byPathDir, scsiDir string) (bool, error) {
	device := connector.MountTargetDevice
	if device == nil || device.Type != "disk" || device.Transport != "iscsi" || len(device.Children) != 0 || len(connector.Devices) != 1 || connector.DeviceWWID == "" || connector.Lun < 0 {
		return false, fmt.Errorf("missing single-path iSCSI device identity")
	}
	path, err := recoveryTargetPath(connector, byPathDir)
	if err != nil {
		return false, err
	}
	if path == "" {
		if !connector.DevicesDetaching {
			return false, fmt.Errorf("iSCSI target path not found before device removal started")
		}
		hctl, err := device.HCTL()
		if err != nil || hctl.HBA < 0 || hctl.Channel < 0 || hctl.Target < 0 || hctl.LUN != int(connector.Lun) || connector.Devices[0].Hctl != device.Hctl {
			return false, fmt.Errorf("missing single-path iSCSI device address")
		}
		entries, err := os.ReadDir(scsiDir)
		if err != nil {
			return false, fmt.Errorf("inspect SCSI devices: %w", err)
		}
		name := fmt.Sprintf("%d:%d:%d:%d", hctl.HBA, hctl.Channel, hctl.Target, hctl.LUN)
		for _, entry := range entries {
			if entry.Name() == name {
				return false, fmt.Errorf("iSCSI device still exists without its target path")
			}
		}
		connector.DevicesDetached = true
		return false, nil
	}
	devices, err := iscsiLib.GetISCSIDevices([]string{path}, true)
	if err != nil {
		return false, fmt.Errorf("inspect iSCSI recovery device: %w", err)
	}
	if len(devices) != 1 {
		return false, fmt.Errorf("target path is ambiguous")
	}
	connector.MountTargetDevice = &devices[0]
	connector.Devices = devices
	return verifyRecoveryDevice(connector, connector.MountTargetDevice.GetPath(), path)
}

func verifyRecoveryDevice(connector *iscsiLib.Connector, devicePath, targetPath string) (bool, error) {
	device := connector.MountTargetDevice
	if device == nil || device.Type != "disk" || device.Transport != "iscsi" || len(device.Children) != 0 || len(connector.Devices) != 1 || connector.DeviceWWID == "" || connector.Lun < 0 {
		return false, fmt.Errorf("missing single-path iSCSI device identity")
	}
	info, err := os.Stat(devicePath)
	if err != nil {
		return false, fmt.Errorf("inspect iSCSI device: %w", err)
	}
	if info.Mode()&os.ModeType != os.ModeDevice {
		return false, fmt.Errorf("iSCSI device is not a block device")
	}
	linked, err := os.Stat(targetPath)
	if err != nil || !os.SameFile(info, linked) {
		return false, fmt.Errorf("iSCSI target path does not match the device")
	}
	if err := verifyRecoveryWWID(connector); err != nil {
		return false, err
	}
	return recoveryDeviceMounted(info)
}

func verifyRecoveryWWID(connector *iscsiLib.Connector) error {
	wwid, err := connector.MountTargetDevice.WWID()
	if err != nil {
		return fmt.Errorf("read iSCSI device WWID: %w", err)
	}
	if wwid != connector.DeviceWWID {
		return fmt.Errorf("iSCSI device WWID changed")
	}
	return nil
}

func recoveryTargetPath(connector *iscsiLib.Connector, byPathDir string) (string, error) {
	entries, err := os.ReadDir(byPathDir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect iSCSI paths: %w", err)
	}
	var path string
	var info os.FileInfo
	for _, portal := range connector.TargetPortals {
		portal, err := validatePortal(portal)
		if err != nil || connector.TargetIqn == "" || strings.Contains(connector.TargetIqn, "/") {
			return "", fmt.Errorf("invalid iSCSI target identity")
		}
		name := fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", portal, connector.TargetIqn, connector.Lun)
		for _, entry := range entries {
			if entry.Name() != name && !(strings.HasPrefix(entry.Name(), "pci-") && strings.HasSuffix(entry.Name(), "-"+name)) {
				continue
			}
			if entry.Type()&os.ModeSymlink == 0 {
				return "", fmt.Errorf("invalid iSCSI device path %s", entry.Name())
			}
			candidate := filepath.Join(byPathDir, entry.Name())
			linked, err := os.Stat(candidate)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("inspect iSCSI target path %s: %w", entry.Name(), err)
			}
			if info != nil && !os.SameFile(info, linked) {
				return "", fmt.Errorf("iSCSI target paths do not match the same device")
			}
			path, info = candidate, linked
		}
	}
	return path, nil
}

func unsharedPortals(infoPath string, connector *iscsiLib.Connector) ([]string, bool, error) {
	entries, err := os.ReadDir(filepath.Dir(infoPath))
	if err != nil {
		return nil, false, err
	}
	portals := slices.Clone(connector.TargetPortals)
	shared := false
	for _, entry := range entries {
		if entry.Name() == filepath.Base(infoPath) || !strings.HasPrefix(entry.Name(), "iscsi-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(infoPath), entry.Name()))
		if err != nil {
			return nil, false, err
		}
		var other iscsiLib.Connector
		if err := json.Unmarshal(data, &other); err != nil || other.TargetIqn == "" || len(other.TargetPortals) == 0 {
			return nil, false, fmt.Errorf("invalid iSCSI connection record %s", entry.Name())
		}
		if other.TargetIqn == connector.TargetIqn {
			shared = true
			portals = slices.DeleteFunc(portals, func(portal string) bool { return slices.Contains(other.TargetPortals, portal) })
		}
	}
	return portals, shared, nil
}

func detachTargetdDisk(c iscsiDiskUnmounter, targetPath, iscsiInfoPath, device string, recorded *iscsiLib.Connector) error {
	if err := validateAttachedDevices(recorded, "/dev"); err != nil {
		if !errors.Is(err, errDevicesGone) {
			return err
		}
		if device != "" {
			return fmt.Errorf("cannot verify mounted device after iSCSI devices disappeared")
		}
		return finishDetach(targetPath, iscsiInfoPath)
	}
	connector, err := iscsiLib.GetConnectorFromFile(iscsiInfoPath)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if connector.VolumeUUID != "" {
		if err := verifyVolumeSerial(connector.Devices, connector.VolumeUUID, hostDeviceSerial); err != nil {
			return err
		}
	}
	if connector.IsMultipathEnabled() {
		if err := connector.IsMultipathConsistent(); err != nil {
			return fmt.Errorf("cannot verify multipath device identity: %w", err)
		}
	}
	if device != "" {
		if _, err := mountedTarget(targetPath, connector.MountTargetDevice.GetPath()); err != nil {
			return err
		}
		if err = c.mounter.Unmount(targetPath); err != nil {
			klog.Errorf("iscsi detach disk: failed to unmount: %s\nError: %v", targetPath, err)
			return err
		}
	}
	device = connector.MountTargetDevice.GetPath()
	inUse, err := deviceMounted(c.mounter, device)
	if err != nil {
		return err
	}
	if inUse {
		klog.Infof("the device is still in use: %s", device)
		return nil
	}

	klog.Info("detaching ISCSI device")
	connector.DevicesDetached = false // The verified device is present again.
	if err := removeVolumeDevices(connector, iscsiInfoPath); err != nil {
		return err
	}
	// Other host consumers can still use this targetd session.
	return finishDetach(targetPath, iscsiInfoPath)
}

func finishDetach(targetPath, infoPath string) error {
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(infoPath)
}

var errDevicesGone = errors.New("recorded iSCSI devices are already gone")

// Recheck recorded devices against the kernel's target/LUN links before removal.
func validateAttachedDevices(connector *iscsiLib.Connector, devRoot string) error {
	if connector.TargetIqn == "" || strings.ContainsAny(connector.TargetIqn, " /\\\t\r\n*?[]") || len(connector.TargetPortals) == 0 || len(connector.Devices) == 0 || connector.MountTargetDevice == nil {
		return fmt.Errorf("invalid iSCSI connection record")
	}
	if connector.MountTargetDevice.Name == "" || filepath.Base(connector.MountTargetDevice.Name) != connector.MountTargetDevice.Name {
		return fmt.Errorf("invalid recorded mount device name")
	}
	links := make(map[string]bool)
	for _, portal := range connector.TargetPortals {
		portal, err := validatePortal(portal)
		if err != nil {
			return err
		}
		suffix := fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", portal, connector.TargetIqn, connector.Lun)
		candidates := []string{filepath.Join(devRoot, "disk/by-path", suffix)}
		pci, err := filepath.Glob(filepath.Join(devRoot, "disk/by-path", "pci-*-"+suffix))
		if err != nil {
			return err
		}
		candidates = append(candidates, pci...)
		for _, link := range candidates {
			path, err := filepath.EvalSymlinks(link)
			if err == nil {
				links[path] = true
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("cannot verify iSCSI device %s: %w", link, err)
			}
		}
	}
	if len(links) == 0 {
		paths := append([]iscsiLib.Device{*connector.MountTargetDevice}, connector.Devices...)
		allGone := true
		for _, device := range paths {
			if filepath.Base(device.Name) != device.Name || device.Name == "" {
				return fmt.Errorf("invalid recorded device name")
			}
			path := filepath.Join(devRoot, device.Name)
			if device.Type == "mpath" {
				path = filepath.Join(devRoot, "mapper", device.Name)
			}
			if _, err := os.Stat(path); err == nil {
				allGone = false
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if allGone {
			return errDevicesGone
		}
	}
	for _, device := range connector.Devices {
		if filepath.Base(device.Name) != device.Name || device.Name == "" {
			return fmt.Errorf("invalid recorded device name")
		}
		path, err := filepath.EvalSymlinks(filepath.Join(devRoot, device.Name))
		if err != nil || !links[path] {
			return fmt.Errorf("recorded iSCSI device %s no longer matches its target and LUN", device.Name)
		}
	}
	if connector.MountTargetDevice.Type != "mpath" {
		path, err := filepath.EvalSymlinks(filepath.Join(devRoot, connector.MountTargetDevice.Name))
		if err != nil || !links[path] {
			return fmt.Errorf("recorded mount device no longer matches its target and LUN")
		}
	}
	return nil
}

func deviceMounted(mounter mount.Interface, device string) (bool, error) {
	mounts, err := mounter.List()
	if err != nil {
		return false, err
	}
	resolvedDevice, _ := filepath.EvalSymlinks(device)
	for _, m := range mounts {
		if m.Device == device {
			return true, nil
		}
		if resolvedDevice != "" {
			if resolvedMount, err := filepath.EvalSymlinks(m.Device); err == nil && resolvedMount == resolvedDevice {
				return true, nil
			}
		}
	}
	info, err := os.Stat(device)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeType != os.ModeDevice {
		return false, fmt.Errorf("%s is not a block device", device)
	}
	return recoveryDeviceMounted(info)
}

func getIscsiInfoPath(volumeID string) string {
	runPath := fmt.Sprintf("/var/run/%s", driverName)

	return fmt.Sprintf("%s/iscsi-%s.json", runPath, volumeID)
}
