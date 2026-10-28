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

var _ = Describe("Unsupported feature gates", func() {
	run := func(selection string) (commandResult, string) {
		tmp := GinkgoT().TempDir()
		log := filepath.Join(tmp, "unexpected-commands")
		fake := filepath.Join(tmp, "fake")
		writeFile(fake, "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$GATE_COMMAND_LOG\"\nexit 93\n", 0755)
		for _, name := range []string{"kubectl", "docker", "make", "git", "go", "kind"} {
			Expect(os.Symlink(fake, filepath.Join(tmp, name))).To(Succeed())
		}
		result := runCommandTimeout(10*time.Second, map[string]string{
			"PATH": tmp + string(os.PathListSeparator) + os.Getenv("PATH"), "GATE_COMMAND_LOG": log,
			"E2E_SCENARIOS": selection, "E2E_RUN_ID": "feature-gate", "NAMESPACE": "feature-gate",
			"E2E_COMMAND_TIMEOUT": "5", "E2E_REQUEST_TIMEOUT": "5", "E2E_TEARDOWN_TIMEOUT": "5",
			"E2E_SCENARIO_TIMEOUT": "5", "E2E_SHARED_IO_TIMEOUT": "5", "E2E_RETAIN_ON_FAILURE": "0",
		}, "bash", filepath.Join(repoRoot, "test/e2e/run-test.sh"))
		data, err := os.ReadFile(log)
		if !os.IsNotExist(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		return result, string(data)
	}
	noTools := func(result commandResult, calls string) {
		Expect(calls).To(BeEmpty(), "selection must fail before tool invocation")
		Expect(result.Stdout + result.Stderr).NotTo(ContainSubstring("E2E test PASSED"))
		Expect(result.Stdout + result.Stderr).NotTo(ContainSubstring("data check passed"))
		Expect(result.Stdout).NotTo(ContainSubstring("=== iSCSI CSI Driver E2E Test ==="))
	}
	DescribeTable("reports each known gate with a reason and distinct failure status", func(selection, reason string) {
		result, calls := run(selection)
		Expect(result.Code).To(Equal(78), result.Stderr)
		Expect(result.Stderr).To(ContainSubstring("Feature gate " + selection + ":"))
		Expect(result.Stderr).To(ContainSubstring(reason))
		Expect(result.Stderr).To(ContainSubstring("No scenarios ran"))
		noTools(result, calls)
	},
		Entry("CHAP secret reference", "chap-secret-ref", "nodePublishSecretRef"),
		Entry("volume statistics", "stats", "NodeGetVolumeStats"),
		Entry("raw block", "raw-block", "raw block publication"),
		Entry("expansion", "expansion", "NodeExpandVolume"),
		Entry("multipath", "multipath", "distinct paths"),
	)
	DescribeTable("rejects mixed executable and gated selections atomically", func(selection string) {
		result, calls := run(selection)
		Expect(result.Code).To(Equal(78), result.Stderr)
		noTools(result, calls)
	}, Entry("baseline then gate", "baseline,stats"), Entry("gate then republish", "stats,republish"), Entry("shared target then raw block", "shared-target,raw-block"), Entry("two gates", "stats,expansion"))
	DescribeTable("reports unknown selections as input errors even alongside gates", func(selection string) {
		result, calls := run(selection)
		Expect(result.Code).To(Equal(2), result.Stderr)
		Expect(result.Stderr).To(ContainSubstring("Unknown E2E_SCENARIOS entry:"))
		Expect(result.Stderr).NotTo(ContainSubstring("No scenarios ran"))
		noTools(result, calls)
	}, Entry("unknown", "made-up"), Entry("gate first", "stats,made-up"), Entry("unknown first", "made-up,stats"), Entry("all with gate", "all,stats"))
	for _, separator := range []string{"\n", "\r", "\r\n", "\t", " "} {
		for _, trailing := range []string{"stats", "made-up"} {
			It("rejects whitespace before trailing "+trailing+" ("+strings.ReplaceAll(strings.ReplaceAll(separator, "\n", "LF"), "\r", "CR")+")", func() {
				result, calls := run("baseline" + separator + trailing)
				Expect(result.Code).To(Equal(2), result.Stderr)
				Expect(result.Stderr).To(ContainSubstring("without whitespace"))
				Expect(result.Stderr).NotTo(ContainSubstring("No scenarios ran"))
				noTools(result, calls)
			})
		}
	}
	It("keeps all limited to executable restart cases", func() {
		result := runCommandTimeout(10*time.Second, nil, "bash", "-euc", `source test/e2e/scenarios.sh; E2E_SCENARIOS=all; validate_scenarios; printf '%s\n' "${SELECTED_SCENARIOS[@]}"`)
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(result.Stdout).To(Equal("republish\ndriver-restart\ntarget-restart\n"))
		Expect(result.Stderr).To(BeEmpty())
	})
})
