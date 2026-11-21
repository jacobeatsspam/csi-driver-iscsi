/*
Copyright 2026 The Kubernetes Authors.

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

package iscsilib

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPersistConnectorPrivateAndAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connection.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := (&Connector{TargetIqn: "iqn.test"}).Persist(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("connector permissions %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || string(data) == "old" {
		t.Fatalf("persisted data %q, %v", data, err)
	}
}

func TestRescanTimeoutDoesNotLogOutSharedSession(t *testing.T) {
	oldExec, oldStat, oldCommand := execWithTimeout, osStat, execCommand
	t.Cleanup(func() { execWithTimeout, osStat, execCommand = oldExec, oldStat, oldCommand })
	execCommand = func(command string, args ...string) *exec.Cmd {
		t.Errorf("unexpected direct command: %s %v", command, args)
		return exec.Command("true")
	}
	var calls [][]string
	execWithTimeout = func(command string, args []string, timeout time.Duration) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if slices.Contains(args, "-R") {
			if !slices.Contains(args, "10.0.0.1:4321") {
				t.Fatalf("rescan lost the configured port: %v", args)
			}
			return nil, os.ErrDeadlineExceeded
		}
		if slices.Contains(args, "session") {
			return []byte("tcp: [1] 10.0.0.1:4321,1 iqn.1993-08.org.debian:one\n"), nil
		}
		t.Fatalf("unexpected command: %s %v", command, args)
		return nil, nil
	}
	osStat = func(string) (os.FileInfo, error) { return nil, nil }
	connector := &Connector{Lun: 1}
	if _, err := connector.connectTarget("iqn.1993-08.org.debian:one", "10.0.0.1:4321", "default", "tcp"); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if slices.Contains(call, "-u") {
			t.Fatal("rescan timeout logged out a shared session")
		}
	}
}

func TestSessionFailureDoesNotChangeSession(t *testing.T) {
	oldExec := execWithTimeout
	t.Cleanup(func() { execWithTimeout = oldExec })
	var calls [][]string
	execWithTimeout = func(command string, args []string, timeout time.Duration) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if slices.Contains(args, "session") {
			return nil, errors.New("session query failed")
		}
		return nil, nil
	}
	connector := &Connector{Lun: 1}
	if _, err := connector.connectTarget("iqn.1993-08.org.debian:one", "10.0.0.1:3260", "default", "tcp"); err == nil {
		t.Fatal("expected error")
	}
	for _, call := range calls {
		if slices.Contains(call, "-l") || slices.Contains(call, "delete") {
			t.Fatalf("unexpected session mutation: %v", call)
		}
	}
}

func TestFailedLoginDoesNotDeleteNodeRecord(t *testing.T) {
	oldExec := execWithTimeout
	t.Cleanup(func() { execWithTimeout = oldExec })
	var calls [][]string
	execWithTimeout = func(command string, args []string, timeout time.Duration) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, errors.New("login failed")
	}
	if err := Login("iqn.test", "10.0.0.1:3260"); err == nil {
		t.Fatal("expected login error")
	}
	if len(calls) != 1 || slices.Contains(calls[0], "delete") {
		t.Fatalf("unexpected node record mutation: %v", calls)
	}
}

func TestInvalidMultipathDoesNotRemoveDevices(t *testing.T) {
	oldExec, oldStat, oldCommand, oldOpen := execWithTimeout, osStat, execCommand, osOpenFile
	t.Cleanup(func() { execWithTimeout, osStat, execCommand, osOpenFile = oldExec, oldStat, oldCommand, oldOpen })
	execWithTimeout = func(command string, args []string, timeout time.Duration) ([]byte, error) {
		if slices.Contains(args, "iface") {
			return []byte("iface.transport_name = tcp\n"), nil
		}
		if slices.Contains(args, "session") {
			return []byte("tcp: [1] 10.0.0.1:3260,1 iqn.1993-08.org.debian:one\ntcp: [2] 10.0.0.2:3260,1 iqn.1993-08.org.debian:one\n"), nil
		}
		return nil, nil
	}
	osStat = func(string) (os.FileInfo, error) { return nil, nil }
	execCommand = func(command string, args ...string) *exec.Cmd {
		if command != "lsblk" {
			t.Errorf("unexpected command %s", command)
			return exec.Command("true")
		}
		return exec.Command("sh", "-c", "printf 'sda sda  1:0:0:1 disk iscsi 1G\\nsdb sdb  2:0:0:1 disk iscsi 1G\\n'")
	}
	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		t.Errorf("attempted to modify device %s", name)
		return nil, errors.New("unexpected device mutation")
	}
	connector := &Connector{TargetIqn: "iqn.1993-08.org.debian:one", TargetPortals: []string{"10.0.0.1:3260", "10.0.0.2:3260"}, Lun: 1}
	if _, err := connector.Connect(); err == nil {
		t.Fatal("expected invalid multipath error")
	}
}

func TestLSBLKRejectsTruncatedDevice(t *testing.T) {
	skipWindows(t)
	previous := execCommand
	t.Cleanup(func() { execCommand = previous })
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("echo", "sda sda  1:0:0:1 disk iscsi")
	}
	if _, err := lsblk([]string{"/dev/sda"}, true); err == nil {
		t.Fatal("accepted a truncated device row")
	}
}

func TestReloadRejectsNewDeviceHolders(t *testing.T) {
	skipWindows(t)
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	var listing string
	execCommand = func(string, ...string) *exec.Cmd {
		return exec.Command("printf", "%s", listing)
	}
	path := filepath.Join(t.TempDir(), "connector.json")
	disk := Device{Name: "sda", Type: "disk", Transport: "iscsi"}
	connector := Connector{MountTargetDevice: &disk, Devices: []Device{disk}}
	if err := connector.Persist(path); err != nil {
		t.Fatal(err)
	}
	listing = "sda sda  1:0:0:1 disk iscsi 1G\n"
	if _, err := GetConnectorFromFile(path); err != nil {
		t.Fatalf("rejected bare disk: %v", err)
	}
	listing += "sda1 sda1 sda  part  1G\n"
	if _, err := GetConnectorFromFile(path); err == nil {
		t.Fatal("accepted a disk with a partition")
	}
	listing = "sda sda  1:0:0:1 disk iscsi 1G\nsdb sdb  2:0:0:1 disk iscsi 1G\nmpatha dm-1 sda  mpath  1G\nmpatha dm-1 sdb  mpath  1G\nmapchild dm-2 dm-1  lvm  512M\n"
	connector.Devices = append(connector.Devices, Device{Name: "sdb", Type: "disk", Transport: "iscsi"})
	connector.MountTargetDevice = &Device{Name: "mpatha", Type: "mpath"}
	if err := connector.Persist(path); err != nil {
		t.Fatal(err)
	}
	if _, err := GetConnectorFromFile(path); err == nil {
		t.Fatal("accepted a multipath map with an LVM holder")
	}
}
