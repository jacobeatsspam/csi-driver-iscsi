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

package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const cleanupIQN = "iqn.2026-01.com.test:storage-run123"

var _ = Describe("Node volume cleanup observations", func() {
	var root string
	BeforeEach(func() {
		root = GinkgoT().TempDir()
		for _, path := range []string{"proc/1", "proc/self", "sys/class/block", "sys/class/iscsi_session", "dev/disk/by-path"} {
			Expect(os.MkdirAll(filepath.Join(root, path), 0755)).To(Succeed())
		}
		writeFile(filepath.Join(root, "proc/1/mountinfo"), "", 0644)
		writeFile(filepath.Join(root, "proc/self/mountinfo"), "", 0644)
	})
	check := func(mode, tracked, volume, lun string) commandResult {
		return runCommandTimeout(10*time.Second, nil, "sh", filepath.Join(repoRoot, "test/e2e/node-volume-clean.sh"), mode, cleanupIQN, volume, tracked, root, lun)
	}
	session := func(name, iqn, device string) string {
		directory := filepath.Join(root, "sys/class/iscsi_session", name)
		Expect(os.Mkdir(directory, 0755)).To(Succeed())
		writeFile(filepath.Join(directory, "targetname"), iqn+"\n", 0644)
		Expect(os.MkdirAll(filepath.Join(directory, "device/target1:0:0/1:0:0:1/block", device), 0755)).To(Succeed())
		Expect(os.Mkdir(filepath.Join(root, "sys/class/block", device), 0755)).To(Succeed())
		return directory
	}
	It("snapshots only devices owned by this target", func() {
		session("session1", cleanupIQN, "sdb")
		session("session2", "iqn.elsewhere:unrelated", "sdz")
		result := check("snapshot", "", "iscsi-pv-run123", "")
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(strings.TrimSpace(result.Stdout)).To(Equal("sdb"))
	})
	It("checks the node mount namespace rather than the validator namespace", func() {
		mount := "1 0 8:0 / /var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/iscsi-pv-run123/mount rw - ext4 /dev/sdb rw\n"
		writeFile(filepath.Join(root, "proc/1/mountinfo"), mount, 0644)
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(1))
		Expect(check("published", "", "iscsi-pv-run123", "").Code).To(Equal(0))
		writeFile(filepath.Join(root, "proc/1/mountinfo"), "", 0644)
		Expect(check("published", "", "iscsi-pv-run123", "").Code).To(Equal(2))
		writeFile(filepath.Join(root, "proc/self/mountinfo"), mount, 0644)
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(0))
	})
	It("rejects owned sessions and stale paths while preserving unrelated state", func() {
		session("session2", "iqn.elsewhere:unrelated", "sdz")
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(0))
		directory := session("session1", cleanupIQN, "sdb")
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(1))
		writeFile(filepath.Join(directory, "targetname"), "iqn.elsewhere:unrelated\n", 0644)
		path := filepath.Join(root, "dev/disk/by-path/ip-10.244.0.17:3260-iscsi-"+cleanupIQN+"-lun-1")
		Expect(os.Symlink("/not/a/device", path)).To(Succeed())
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(1))
		Expect(os.Remove(path)).To(Succeed())
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(0))
		Expect(filepath.Join(root, "sys/class/block/sdz")).To(BeADirectory())
	})
	It("requires captured devices to disappear after the session is removed", func() {
		session("session1", cleanupIQN, "sdb")
		captured := strings.TrimSpace(check("snapshot", "", "iscsi-pv-run123", "").Stdout)
		Expect(os.Remove(filepath.Join(root, "sys/class/iscsi_session/session1/targetname"))).To(Succeed())
		Expect(check("check", captured, "iscsi-pv-run123", "").Code).To(Equal(1))
		Expect(os.Remove(filepath.Join(root, "sys/class/block/sdb"))).To(Succeed())
		Expect(check("check", captured, "iscsi-pv-run123", "").Code).To(Equal(0))
	})
	It("allows another LUN and its shared session to remain", func() {
		directory := session("session1", cleanupIQN, "sdb")
		Expect(os.MkdirAll(filepath.Join(directory, "device/target1:0:0/1:0:0:2/block/sdc"), 0755)).To(Succeed())
		Expect(os.Mkdir(filepath.Join(root, "sys/class/block/sdc"), 0755)).To(Succeed())
		Expect(strings.TrimSpace(check("snapshot-lun", "", "iscsi-pv-run123", "1").Stdout)).To(Equal("sdb"))
		Expect(strings.TrimSpace(check("snapshot-lun", "", "iscsi-pv-run123", "2").Stdout)).To(Equal("sdc"))
		Expect(os.Symlink("/dev/sdc", filepath.Join(root, "dev/disk/by-path/ip-10.0.0.1-iscsi-"+cleanupIQN+"-lun-2"))).To(Succeed())
		Expect(check("check-lun", "sdb", "iscsi-pv-run123", "1").Code).To(Equal(1))
		Expect(os.Remove(filepath.Join(root, "sys/class/block/sdb"))).To(Succeed())
		Expect(check("check-lun", "sdb", "iscsi-pv-run123", "1").Code).To(Equal(0))
		Expect(filepath.Join(directory, "targetname")).To(BeAnExistingFile())
		Expect(filepath.Join(root, "sys/class/block/sdc")).To(BeADirectory())
		Expect(check("check-lun", "", "iscsi-pv-run123", "bad").Code).To(Equal(2))
	})
	It("does not hide another publication's mount after device removal", func() {
		writeFile(filepath.Join(root, "proc/1/mountinfo"), "1 0 8:1 / /var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/iscsi-pv-b/mount rw - ext4 /dev/sdc rw\n", 0644)
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(0))
		Expect(check("check", "", "iscsi-pv-b", "").Code).To(Equal(1))
	})
	It("reports unsafe device names and unobservable mount state as errors", func() {
		Expect(check("check", "../../etc", "iscsi-pv-run123", "").Code).To(Equal(2))
		Expect(check("check", "*.md", "iscsi-pv-run123", "").Code).To(Equal(2))
		Expect(os.Remove(filepath.Join(root, "proc/1/mountinfo"))).To(Succeed())
		Expect(check("check", "", "iscsi-pv-run123", "").Code).To(Equal(2))
	})
})
