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
	"encoding/json"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func init() { commandFakes["harness"] = harnessFakeCommand }

func harnessFakeCommand(args []string) int {
	name := filepath.Base(os.Args[0])
	if name == "kubectl" {
		filtered := []string{}
		for _, arg := range args {
			if !strings.HasPrefix(arg, "--request-timeout=") {
				filtered = append(filtered, arg)
			}
		}
		args = filtered
	}
	if err := appendCommand(os.Getenv("COMMAND_LOG"), append([]string{name}, args...)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 90
	}
	failure := os.Getenv("FAILURE")
	prefix := func(parts ...string) bool { return slices.Equal(args[:min(len(args), len(parts))], parts) }
	emit := func(value any) { data, _ := json.Marshal(value); fmt.Println(string(data)) }
	switch name {
	case "go":
		fmt.Println("/tmp/iscsi-test-go")
	case "git":
		fmt.Println(strings.Repeat("b", 40))
	case "nc":
		return 1
	case "docker":
		if prefix("image", "inspect") {
			fmt.Println("sha256:" + strings.Repeat("a", 64))
		}
	case "kubectl":
		switch {
		case prefix("config", "current-context"):
			fmt.Println("kind-test")
		case prefix("create") || prefix("apply"):
			var data []byte
			var err error
			if len(args) < 3 {
				return 91
			}
			if args[2] == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(args[2])
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 92
			}
			if err = appendCommand(os.Getenv("COMMAND_LOG"), []string{"manifest", string(data)}); err != nil {
				return 93
			}
			if failure == "apply" && strings.Contains(string(data), "kind: PersistentVolume\n") {
				return 17
			}
			if kind := os.Getenv("FAIL_CREATE_KIND"); kind != "" && strings.Contains(string(data), "kind: "+kind+"\n") {
				return 19
			}
		case prefix("get") && slices.Contains(args, "--ignore-not-found"):
			if failure == "preflight" {
				return 27
			}
			if os.Getenv("EXISTING_RESOURCE") == args[1] {
				fmt.Println(args[1])
			}
			if os.Getenv("STALE_RUN") == args[1] {
				if args[1] == "namespace,pv,csidriver" {
					fmt.Println("namespace/earlier-run\npersistentvolume/earlier-pv")
				} else {
					fmt.Println("configmap/earlier-driver")
				}
			}
		case prefix("get", "service"):
			fmt.Println("10.96.0.17")
		case prefix("get", "daemonset"):
			ready := 1
			if failure == "no-nodes" {
				ready = 0
			}
			emit(map[string]any{"status": map[string]int{"desiredNumberScheduled": ready, "numberReady": ready}})
		case prefix("run"):
			if failure == "hang" {
				time.Sleep(5 * time.Second)
			}
			if failure == "signal" {
				_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGTERM)
				time.Sleep(time.Second)
			}
			if failure == "connectivity" {
				index := slices.Index(args, "--")
				if index < 0 || index+1 >= len(args) {
					return 94
				}
				cmd := exec.Command(args[index+1], args[index+2:]...)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					if exit, ok := err.(*exec.ExitError); ok {
						return exit.ExitCode()
					}
					return 95
				}
			}
		case prefix("wait") && failure == "timeout":
			return 124
		case prefix("rollout", "status") && failure == "node-prerequisites":
			for _, arg := range args {
				if strings.Contains(arg, "iscsi-init-") {
					return 37
				}
			}
		case prefix("exec"):
			if slices.Contains(args, "volume-clean") {
				if failure == "teardown-observe" {
					return 2
				}
				if slices.Contains(args, "snapshot") {
					fmt.Println("sdb")
				} else if failure == "teardown-residual" {
					return 1
				}
				return 0
			}
			if failure == "exec" {
				return 23
			}
			if !slices.Contains(args, "seed") {
				if failure == "payload" {
					fmt.Println("secret-sentinel")
				} else {
					fmt.Println("iSCSI CSI test successful")
				}
			}
		case prefix("get", "pods") && len(args) > 0 && args[len(args)-1] == "json":
			selector := ""
			if index := slices.Index(args, "-l"); index >= 0 && index+1 < len(args) {
				selector = args[index+1]
			}
			if strings.HasPrefix(selector, "app=iscsi-target-") {
				image, ip, ready := "iscsi-target:test", "10.244.0.17", "True"
				if failure == "target-wrong-image" {
					image = "wrong-target:test"
				}
				if failure == "target-bad-ip" {
					ip = "not-an-ip"
				}
				if failure == "target-unready" {
					ready = "False"
				}
				target := map[string]any{"metadata": map[string]string{"name": "target-test"}, "spec": map[string]any{"nodeName": "node-test", "containers": []any{map[string]string{"name": "tgtd", "image": image}}}, "status": map[string]any{"containerStatuses": []any{map[string]any{"name": "tgtd", "ready": true, "imageID": "sha256:" + strings.Repeat("a", 64)}}, "podIP": ip, "conditions": []any{map[string]string{"type": "Ready", "status": ready}}}}
				items := []any{target}
				if failure == "target-multiple" {
					items = append(items, target)
				}
				emit(map[string]any{"items": items})
				return 0
			}
			if strings.HasPrefix(selector, "app=iscsi-init-") {
				items := []any{}
				if failure != "teardown-no-validators" {
					items = append(items, map[string]any{"metadata": map[string]string{"name": "validator-test"}, "spec": map[string]string{"nodeName": "node-test"}, "status": map[string]any{"conditions": []any{map[string]string{"type": "Ready", "status": "True"}}}})
				}
				emit(map[string]any{"items": items})
				return 0
			}
			emit(map[string]any{"items": []any{map[string]any{"metadata": map[string]string{"name": "driver-test"}, "spec": map[string]any{"containers": []any{map[string]string{"name": "iscsi", "image": "iscsi-e2e:test"}}}, "status": map[string]any{"containerStatuses": []any{map[string]any{"name": "iscsi", "ready": true, "imageID": "sha256:" + strings.Repeat("a", 64)}}}}}})
		case prefix("get") && os.Getenv("FAIL_DIAGNOSTICS") != "":
			return 31
		case prefix("get", "events") && failure == "node-prerequisites" && slices.Contains(args, "--field-selector"):
			fmt.Println("Unhealthy Node prerequisite failed: iscsid is not running")
		case prefix("delete") && os.Getenv("FAIL_CLEANUP") != "":
			return 35
		}
	}
	return 0
}

type harnessResult struct {
	commandResult
	Calls     [][]string
	Artifacts []os.DirEntry
}

func runHarness(overrides map[string]string) harnessResult {
	GinkgoHelper()
	dir, log := newFakeCommands("harness", "kubectl", "docker", "git", "make", "kind", "sleep", "nc", "go")
	runtime := filepath.Join(dir, "runtime")
	Expect(os.Mkdir(runtime, 0700)).To(Succeed())
	env := map[string]string{"E2E_GO_FAKE": "harness", "PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "COMMAND_LOG": log, "FAILURE": "", "NAMESPACE": "test-run", "TMPDIR": runtime, "E2E_RETAIN_ON_FAILURE": "0", "FAIL_CLEANUP": "", "FAIL_CREATE_KIND": "", "E2E_RUN_ID": "run123", "EXISTING_RESOURCE": "", "STALE_RUN": "", "FAIL_DIAGNOSTICS": "", "E2E_COMMAND_TIMEOUT": "330", "E2E_TEARDOWN_TIMEOUT": "1", "E2E_DRIVER_IMAGE": "iscsi-e2e:test", "E2E_TARGET_IMAGE": "iscsi-target:test", "E2E_KIND_CLUSTER": "test", "E2E_ARCH": "amd64", "E2E_DRIVER_DELIVERY": "kind", "E2E_SCENARIOS": "baseline"}
	for key, value := range overrides {
		env[key] = value
	}
	var result commandResult
	if target := env["MAKE_TARGET"]; target != "" {
		makePath, err := exec.LookPath("make")
		Expect(err).NotTo(HaveOccurred())
		result = runCommand(env, makePath, target, "ISCSI_SERVER_NAMESPACE="+env["NAMESPACE"])
	} else {
		result = runCommand(env, "bash", filepath.Join(repoRoot, "test/e2e/run-test.sh"))
	}
	artifacts, err := os.ReadDir(runtime)
	Expect(err).NotTo(HaveOccurred())
	return harnessResult{result, readCalls(log), artifacts}
}
func hMatches(calls [][]string, predicate func([]string) bool) [][]string {
	var found [][]string
	for _, c := range calls {
		if predicate(c) {
			found = append(found, c)
		}
	}
	return found
}
func hPrefix(call []string, prefix ...string) bool {
	return len(call) >= len(prefix) && slices.Equal(call[:len(prefix)], prefix)
}
func hManifest(calls [][]string, needle string) string {
	GinkgoHelper()
	for _, c := range calls {
		if hPrefix(c, "manifest") && len(c) > 1 && strings.Contains(c[1], needle) {
			return c[1]
		}
	}
	Fail("missing manifest containing " + needle)
	return ""
}
func hIndex(calls [][]string, predicate func([]string) bool) int {
	GinkgoHelper()
	for i, c := range calls {
		if predicate(c) {
			return i
		}
	}
	Fail("missing expected command")
	return -1
}
func hToken(manifest string) string {
	GinkgoHelper()
	for _, line := range strings.Split(manifest, "\n") {
		if strings.Contains(line, "iscsi.csi.k8s.io/e2e-invocation:") {
			parts := strings.Split(line, "\"")
			Expect(parts).To(HaveLen(3))
			return parts[1]
		}
	}
	Fail("missing invocation token")
	return ""
}
func hNoMutation(calls [][]string) {
	GinkgoHelper()
	Expect(hMatches(calls, func(c []string) bool {
		return hPrefix(c, "kubectl", "create") || hPrefix(c, "kubectl", "delete") || c[0] == "docker" || c[0] == "make"
	})).To(BeEmpty())
}

var _ = Describe("Harness", func() {
	It("completes a successful harness run", func() {
		r := runHarness(nil)
		Expect(r.Code).To(Equal(0), r.Stderr)
		Expect(r.Stdout).To(ContainSubstring("E2E test PASSED"))
		Expect(hasCall(r.Calls, "kubectl", "exec")).To(BeTrue())
	})

	It("stops before driver installation on a closed target port", func() {
		r := runHarness(map[string]string{"FAILURE": "connectivity"})
		Expect(r.Code).To(Equal(1), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("failed during connectivity"))
		Expect(hMatches(r.Calls, func(c []string) bool { return hPrefix(c, "manifest") && strings.Contains(c[1], "kind: CSIDriver") })).To(BeEmpty())
		Expect(r.Stdout).NotTo(ContainSubstring("E2E test PASSED"))
	})

	It("preserves PV creation failure before creating a PVC", func() {
		r := runHarness(map[string]string{"FAILURE": "apply"})
		Expect(r.Code).To(Equal(17), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("failed during pv-create"))
		Expect(hMatches(r.Calls, func(c []string) bool {
			return hPrefix(c, "manifest") && strings.Contains(c[1], "kind: PersistentVolumeClaim")
		})).To(BeEmpty())
	})

	It("stops before data verification on a wait timeout", func() {
		r := runHarness(map[string]string{"FAILURE": "timeout"})
		Expect(r.Code).To(Equal(124), r.Stderr)
		Expect(hasCall(r.Calls, "kubectl", "exec")).To(BeFalse())
	})

	It("preserves the data verification command status", func() {
		r := runHarness(map[string]string{"FAILURE": "exec"})
		Expect(r.Code).To(Equal(23), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("failed during data-verification"))
	})

	It("rejects mismatched data without printing the payload", func() {
		r := runHarness(map[string]string{"FAILURE": "payload"})
		Expect(r.Code).To(Equal(1), r.Stderr)
		Expect(r.Stdout + r.Stderr).NotTo(ContainSubstring("secret-sentinel"))
		Expect(hMatches(r.Calls, func(c []string) bool { return hPrefix(c, "kubectl", "get") && !slices.Contains(c, "json") })).NotTo(BeEmpty())
		Expect(hMatches(r.Calls, func(c []string) bool { return hPrefix(c, "kubectl", "logs") || slices.Contains(c, "describe") })).To(BeEmpty())
	})

	It("bounds a streaming command", func() {
		r := runHarness(map[string]string{"FAILURE": "hang", "E2E_COMMAND_TIMEOUT": "1"})
		Expect(r.Code).To(Equal(124), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("failed during connectivity"))
	})

	It("preserves the original failure if diagnostics fail", func() {
		r := runHarness(map[string]string{"FAILURE": "apply", "FAIL_DIAGNOSTICS": "1"})
		Expect(r.Code).To(Equal(17), r.Stderr)
	})

	It("requires a positive command timeout", func() {
		r := runCommand(map[string]string{"E2E_COMMAND_TIMEOUT": "0"}, "bash", filepath.Join(repoRoot, "test/e2e/run-test.sh"))
		Expect(r.Code).To(Equal(2), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("positive integer"))
	})

})
