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
	"errors"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func init() {
	commandFakes["shared-target"] = sharedTargetFakeCommand
	commandFakes["shared-worker-hang"] = func([]string) int { signal.Ignore(syscall.SIGTERM); time.Sleep(20 * time.Second); return 0 }
	commandFakes["shared-worker-dd"] = func([]string) int { return 7 }
}

type sharedTargetState struct {
	Deleted     bool `json:"deleted"`
	Republished bool `json:"republished"`
	Progress    int  `json:"progress"`
}

func sharedTargetFakeCommand(args []string) int {
	path := os.Getenv("STATE")
	state := sharedTargetState{}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &state) != nil {
			return 90
		}
	}
	if appendCommand(os.Getenv("LOG"), args) != nil {
		return 91
	}
	failure := os.Getenv("FAILURE")
	save := func() { raw, _ := json.Marshal(state); _ = os.WriteFile(path, raw, 0600) }
	emit := func(value any) { raw, _ := json.Marshal(value); fmt.Println(string(raw)) }
	prefix := func(parts ...string) bool { return len(args) >= len(parts) && slices.Equal(args[:len(parts)], parts) }
	switch {
	case prefix("get", "pod"):
		switch args[2] {
		case "validator":
			fmt.Println("node1")
		case "workload":
			if slices.Contains(args, "jsonpath={.metadata.uid}") {
				uid := "old-a"
				if state.Deleted {
					uid = ""
				}
				if state.Republished {
					uid = "new-a"
				}
				fmt.Println(uid)
			} else {
				emit(map[string]any{"metadata": map[string]string{"uid": "old-a"}, "spec": map[string]string{"nodeName": "node1"}})
			}
		default:
			if failure == "delayed-start-api" && !state.Deleted || failure == "delayed-removal-api" && state.Deleted {
				time.Sleep(5 * time.Second)
			}
			restarts := 0
			if failure == "interrupted" && state.Deleted {
				restarts = 1
			}
			emit(map[string]any{"metadata": map[string]string{"uid": "worker-b"}, "spec": map[string]string{"nodeName": "node1"}, "status": map[string]any{"containerStatuses": []any{map[string]any{"name": "test", "restartCount": restarts, "ready": true, "state": map[string]any{"running": map[string]any{}}}}}})
		}
	case prefix("get", "pods"):
		emit(map[string]any{"items": []any{map[string]any{"metadata": map[string]string{"name": "validator"}, "spec": map[string]string{"nodeName": "node1"}, "status": map[string]any{"conditions": []any{map[string]string{"type": "Ready", "status": "True"}}}}}})
	case prefix("get", "daemonset"):
		emit(map[string]any{"status": map[string]int{"desiredNumberScheduled": 1}})
	case prefix("exec"):
		switch {
		case slices.Contains(args, "/tmp/io-progress"):
			state.Progress++
			save()
			if failure == "startup-stall" {
				fmt.Println(1)
			} else {
				fmt.Println(state.Progress)
			}
		case slices.Contains(args, "/mnt/b-pattern"):
			fmt.Println("iSCSI shared target B payload")
		case slices.Contains(args, "/mnt/test-file"):
			fmt.Println("iSCSI CSI test successful")
		case slices.Contains(args, "published"):
			if failure == "unobserved-a" {
				return 2
			}
		case slices.Contains(args, "snapshot-lun"):
			if failure == "no-a-device" {
				fmt.Println("")
			} else {
				fmt.Println("sda")
			}
		case slices.Contains(args, "snapshot"):
			fmt.Println("sda\nsdb")
		case slices.Contains(args, "check-lun"):
			if failure == "retained-a" {
				return 1
			}
		case slices.Contains(args, "check"):
			index := slices.Index(args, "check")
			if failure == "retained-b-mount" && len(args) > index+2 && args[index+2] == "pv-b" {
				return 1
			}
		}
	case prefix("delete", "pod"):
		state.Deleted = true
		save()
	case prefix("create"):
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 92
		}
		if appendCommand(os.Getenv("LOG"), []string{"manifest", string(data)}) != nil {
			return 93
		}
		if strings.Contains(string(data), "name: workload\n") {
			state.Republished = true
			save()
		}
	}
	return 0
}

type sharedTargetResult struct {
	commandResult
	Calls [][]string
}

func runSharedTarget(failure string, cleanup bool) sharedTargetResult {
	GinkgoHelper()
	dir, log := newFakeCommands("shared-target", "kubectl")
	teardown, ioBudget := "5", "10"
	if failure != "" {
		teardown = "1"
	}
	if failure == "startup-stall" || failure == "delayed-start-api" {
		ioBudget = "2"
	}
	env := map[string]string{"E2E_GO_FAKE": "shared-target", "PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "STATE": filepath.Join(dir, "state"), "LOG": log, "FAILURE": failure, "TEARDOWN_TIMEOUT": teardown, "IO_TIMEOUT": ioBudget}
	script := `set -euo pipefail
SCRIPT_DIR=$PWD/test/e2e
source test/e2e/scenarios.sh
source test/e2e/cleanup.sh
kubectl() { timeout --foreground "${E2E_COMMAND_TIMEOUT:-10}s" kubectl "$@"; }
STAGE=scenario-shared-target
NAMESPACE=owned; POD_NAME=workload; PVC_NAME=claim; PV_NAME=pv-a
POD_B_NAME=worker; PVC_B_NAME=claim-b; PV_B_NAME=pv-b
INIT_NAME=init; TARGET_PORTAL=10.0.0.9:3260; IQN=iqn.test:shared
OWNER_LABEL=owner; E2E_RUN_ID=run; CLEANUP_LABEL=invocation; CLEANUP_TOKEN=unique
E2E_SCENARIO_TIMEOUT=20; E2E_SHARED_IO_TIMEOUT=$IO_TIMEOUT; E2E_TEARDOWN_TIMEOUT=$TEARDOWN_TIMEOUT
SCENARIO_DEADLINE=$((SECONDS+20)); EXPECTED_PAYLOAD='iSCSI CSI test successful'
declare -a TEARDOWN_VOLUMES=(pv-a)
declare -a TEARDOWN_VALIDATORS=()
declare -A TEARDOWN_DEVICES=()
`
	if cleanup {
		script += `TEARDOWN_VOLUMES+=(pv-b); TEARDOWN_VALIDATORS=(validator); TEARDOWN_DEVICES[validator]=""; verify_volume_teardown`
	} else {
		script += "shared_target_scenario"
	}
	result := runCommandTimeout(25*time.Second, env, "bash", "-c", script)
	return sharedTargetResult{result, readCalls(log)}
}

var _ = Describe("Shared-target scenarios", func() {
	It("keeps B I/O running until A is actually removed on their shared node", func() {
		r := runSharedTarget("", false)
		Expect(r.Code).To(Equal(0), r.Stderr)
		manifest := r.Calls[scenarioCallIndex(r.Calls, func(c []string) bool { return c[0] == "manifest" })][1]
		for _, text := range []string{"nodeName: node1", "lun: \"2\"", "persistentVolumeReclaimPolicy: Retain", "conv=fsync", "owner: \"run\"", "invocation: \"unique\""} {
			Expect(manifest).To(ContainSubstring(text))
		}
		deletion := scenarioCallIndex(r.Calls, func(c []string) bool { return len(c) >= 2 && c[0] == "delete" && c[1] == "pod" })
		Expect(r.Calls[deletion]).To(ContainElement("--wait=false"))
		for _, calls := range [][][]string{r.Calls[:deletion], r.Calls[deletion+1:]} {
			Expect(len(scenarioCalls(calls, func(c []string) bool { return slices.Contains(c, "/tmp/io-progress") }))).To(BeNumerically(">=", 2))
		}
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "check-lun") })).NotTo(BeEmpty())
		Expect(r.Stdout).To(ContainSubstring("A LUN removed while checked B I/O continued"))
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "seed") })).To(BeEmpty())
	})
	It("does not delete unobserved A or claim its removal", func() {
		for _, failure := range []string{"unobserved-a", "no-a-device"} {
			By(failure)
			r := runSharedTarget(failure, false)
			Expect(r.Code).NotTo(Equal(0))
			Expect(hasCall(r.Calls, "delete", "pod")).To(BeFalse())
			Expect(r.Stdout).NotTo(ContainSubstring("A LUN removed"))
		}
	})
	It("fails if the B worker is interrupted instead of accepting eventual recovery", func() {
		r := runSharedTarget("interrupted", false)
		Expect(r.Code).NotTo(Equal(0))
		Expect(r.Stderr).To(ContainSubstring("B worker interrupted or restarted"))
		Expect(r.Stdout).NotTo(ContainSubstring("A LUN removed"))
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return c[0] == "manifest" })).To(HaveLen(1))
	})
	It("cannot claim success while A remains attached", func() {
		r := runSharedTarget("retained-a", false)
		Expect(r.Code).NotTo(Equal(0))
		Expect(hasCall(r.Calls, "delete", "pod")).To(BeTrue(), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("B survival cannot establish safe removal"))
		Expect(r.Stdout).NotTo(ContainSubstring("A LUN removed"))
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return c[0] == "manifest" })).To(HaveLen(1))
	})
	It("checks the second volume mount marker during final cleanup", func() {
		r := runSharedTarget("retained-b-mount", true)
		Expect(r.Code).NotTo(Equal(0))
		checked := map[string]bool{}
		for _, c := range r.Calls {
			if index := slices.Index(c, "check"); index >= 0 {
				checked[c[index+2]] = true
			}
		}
		Expect(checked).To(Equal(map[string]bool{"pv-a": true, "pv-b": true}))
	})
	It("uses the narrowest deadline for startup stalls and delayed queries", func() {
		for _, failure := range []string{"startup-stall", "delayed-start-api", "delayed-removal-api"} {
			By(failure)
			started := time.Now()
			r := runSharedTarget(failure, false)
			Expect(r.Code).NotTo(Equal(0))
			Expect(time.Since(started)).To(BeNumerically("<", 6*time.Second))
			Expect(r.Stdout).NotTo(ContainSubstring("A LUN removed"))
			if failure == "startup-stall" || failure == "delayed-start-api" {
				Expect(hasCall(r.Calls, "delete", "pod")).To(BeFalse())
			}
		}
	})
	It("bounds worker initial sync and loop reads", func() {
		for _, name := range []string{"sync", "cat"} {
			By(name)
			dir, _ := newFakeCommands("shared-worker-hang", name)
			progress := filepath.Join(dir, "progress")
			env := map[string]string{"E2E_GO_FAKE": "shared-worker-hang", "PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "SHARED_DATA_DIR": dir, "SHARED_PROGRESS_FILE": progress, "SHARED_IO_TIMEOUT": "1"}
			started := time.Now()
			r := runCommandTimeout(5*time.Second, env, "sh", filepath.Join(repoRoot, "test/e2e/shared-io.sh"))
			Expect(r.Code).To(Equal(1), r.Stderr)
			Expect(time.Since(started)).To(BeNumerically("<", 4*time.Second))
			_, err := os.Stat(progress)
			Expect(errors.Is(err, os.ErrNotExist)).To(BeTrue())
		}
	})
	It("does not publish successful progress after a worker dd failure", func() {
		dir, _ := newFakeCommands("shared-worker-dd", "dd")
		progress := filepath.Join(dir, "progress")
		r := runCommandTimeout(5*time.Second, map[string]string{"E2E_GO_FAKE": "shared-worker-dd", "PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "SHARED_DATA_DIR": dir, "SHARED_PROGRESS_FILE": progress}, "sh", filepath.Join(repoRoot, "test/e2e/shared-io.sh"))
		Expect(r.Code).To(Equal(1), r.Stderr)
		Expect(r.Stderr).To(ContainSubstring("B I/O failed"))
		_, err := os.Stat(progress)
		Expect(errors.Is(err, os.ErrNotExist)).To(BeTrue())
	})
})
