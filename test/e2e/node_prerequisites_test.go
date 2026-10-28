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
	"io/fs"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Snapshot regular files so prerequisite checks must remain read-only.
func prerequisiteFiles(root string) map[string]string {
	files := map[string]string{}
	Expect(filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(data)
		}
		return nil
	})).To(Succeed())
	return files
}

var _ = Describe("Node prerequisites", func() {
	check := func(missing, osName string) commandResult {
		root := GinkgoT().TempDir()
		binary := filepath.Join(root, "bin")
		Expect(os.MkdirAll(binary, 0755)).To(Succeed())
		writeFile(filepath.Join(binary, "uname"), "#!/bin/sh\nprintf '%s\\n' \"$TEST_OS\"\n", 0755)
		if missing != "iscsiadm" {
			writeFile(filepath.Join(binary, "iscsiadm"), "#!/bin/sh\nexit 99 # validation must never execute this\n", 0755)
		}
		for i, name := range []string{"iscsid", "systemd-udevd"} {
			if name != missing {
				process := filepath.Join(root, "proc", []string{"1", "2"}[i])
				Expect(os.MkdirAll(process, 0755)).To(Succeed())
				writeFile(filepath.Join(process, "comm"), name+"\n", 0644)
			}
		}
		if missing != "transport" {
			Expect(os.MkdirAll(filepath.Join(root, "sys/class/iscsi_transport/tcp"), 0755)).To(Succeed())
		}
		before := prerequisiteFiles(root)
		result := runCommandTimeout(10*time.Second, map[string]string{"PATH": binary, "TEST_OS": osName}, "/bin/sh", filepath.Join(repoRoot, "test/e2e/node-prerequisites.sh"), root)
		Expect(prerequisiteFiles(root)).To(Equal(before), "node checks changed fixture files")
		return result
	}
	It("passes prepared Linux nodes using read-only checks", func() {
		result := check("", "Linux")
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(result.Stdout).To(ContainSubstring("read-only checks"))
	})
	DescribeTable("rejects missing prerequisites with the specific reason", func(missing, reason string) {
		result := check(missing, "Linux")
		Expect(result.Code).To(Equal(1), result.Stderr)
		Expect(result.Stderr).To(ContainSubstring(reason))
		Expect(result.Stdout).NotTo(ContainSubstring("verified"))
	},
		Entry("iscsiadm", "iscsiadm", "iscsiadm is missing"),
		Entry("iscsid", "iscsid", "iscsid is not running"),
		Entry("transport", "transport", "TCP kernel transport is unavailable"),
		Entry("udev", "systemd-udevd", "udev is not running"),
	)
	It("rejects non-Linux nodes", func() {
		result := check("", "Darwin")
		Expect(result.Code).To(Equal(1))
		Expect(result.Stderr).To(ContainSubstring("Linux nodes are required"))
	})
})
