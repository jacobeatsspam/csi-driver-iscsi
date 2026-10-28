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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func init() { commandFakes["scenario"] = scenarioFakeCommand }

type scenarioState struct {
	Publication int `json:"publication"`
	Driver      int `json:"driver"`
	Target      int `json:"target"`
	Polls       int `json:"polls"`
}

func scenarioFakeCommand(args []string) int {
	filtered := []string{}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--request-timeout=") {
			filtered = append(filtered, arg)
		}
	}
	args = filtered
	path := os.Getenv("SCENARIO_STATE")
	state := scenarioState{}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &state) != nil {
			return 90
		}
	}
	if appendCommand(os.Getenv("COMMAND_LOG"), args) != nil {
		return 91
	}
	failure := os.Getenv("SCENARIO_FAILURE")
	slowDelay := func() {
		delay, err := strconv.ParseFloat(os.Getenv("SCENARIO_SLOW_DELAY"), 64)
		if err != nil {
			delay = .65
		}
		time.Sleep(time.Duration(delay * float64(time.Second)))
	}
	if filepath.Base(os.Args[0]) == "docker" {
		if slices.Contains([]string{"driver-slow-fallbacks", "driver-slow-query", "target-slow-query"}, failure) {
			slowDelay()
		} else {
			time.Sleep(20 * time.Second)
		}
		fmt.Println(`{"status":{"id":"sha256:` + strings.Repeat("a", 64) + `"}}`)
		return 0
	}
	if failure == "driver-query-hang" || failure == "target-query-hang" {
		time.Sleep(20 * time.Second)
	}
	if failure == "driver-slow-query" || failure == "target-slow-query" {
		slowDelay()
	}
	save := func() { raw, _ := json.Marshal(state); _ = os.WriteFile(path, raw, 0600) }
	emit := func(value any) { raw, _ := json.Marshal(value); fmt.Println(string(raw)) }
	prefix := func(parts ...string) bool { return len(args) >= len(parts) && slices.Equal(args[:len(parts)], parts) }
	target := func() map[string]any {
		uid, letter, restarts := "target-old", "a", state.Target
		if failure == "target-pod-change" && state.Target != 0 {
			uid = "target-new"
		}
		if failure == "target-no-restart" {
			restarts = 0
		}
		if failure == "target-identity-hang" || failure == "target-slow-query" {
			letter = "c"
		}
		return map[string]any{"metadata": map[string]string{"name": "target-test", "uid": uid}, "spec": map[string]any{"nodeName": "node-test", "containers": []any{map[string]string{"name": "tgtd", "image": "target:test"}}}, "status": map[string]any{"podIP": "10.42.0.9", "conditions": []any{map[string]string{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "tgtd", "ready": true, "restartCount": restarts, "imageID": "sha256:" + strings.Repeat(letter, 64)}}}}
	}
	switch {
	case prefix("get", "pod"):
		if args[2] == "workload" {
			fmt.Println("publication-" + strconv.Itoa(state.Publication))
		} else {
			state.Polls++
			save()
			emit(target())
		}
	case prefix("get", "pods"):
		index := slices.Index(args, "-l")
		if index < 0 || index+1 >= len(args) {
			return 92
		}
		if args[index+1] == "app=csi-iscsi-node" {
			number := state.Driver
			if failure == "driver-no-restart" {
				number = 0
			}
			letter := "a"
			if slices.Contains([]string{"driver-identity-hang", "driver-slow-fallbacks", "driver-slow-query"}, failure) {
				letter = "c"
			}
			pod := map[string]any{"metadata": map[string]string{"name": "driver-test", "uid": "driver-" + strconv.Itoa(number)}, "spec": map[string]any{"nodeName": "node-test", "containers": []any{map[string]string{"name": "iscsi", "image": "driver:test"}}}, "status": map[string]any{"containerStatuses": []any{map[string]any{"name": "iscsi", "ready": true, "imageID": "sha256:" + strings.Repeat(letter, 64)}}}}
			items := []any{pod}
			if failure == "driver-slow-fallbacks" {
				second := map[string]any{}
				for k, v := range pod {
					second[k] = v
				}
				second["metadata"] = map[string]string{"name": "driver-second", "uid": "driver-second"}
				items = append(items, second)
			}
			emit(map[string]any{"items": items})
		} else {
			emit(map[string]any{"items": []any{target()}})
		}
	case prefix("delete", "pod"):
		if args[2] == "driver-test" {
			state.Driver++
		} else {
			state.Publication++
		}
		save()
	case prefix("create"):
		manifest, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 93
		}
		if appendCommand(os.Getenv("COMMAND_LOG"), []string{"manifest", string(manifest)}) != nil {
			return 94
		}
	case prefix("exec"):
		if slices.Contains(args, "kill -TERM 1") {
			state.Target++
			save()
		} else if slices.Contains(args, "cat") {
			stage := "baseline"
			if state.Publication != 0 {
				stage = "republish"
			}
			if state.Driver != 0 {
				stage = "driver-restart"
			}
			if state.Target != 0 {
				stage = "target-restart"
			}
			if failure == stage {
				fmt.Println("CORRUPT-SENTINEL")
			} else {
				fmt.Println("iSCSI CSI test successful")
			}
		}
	}
	return 0
}

type scenarioResult struct {
	commandResult
	Calls [][]string
}

func runScenario(overrides map[string]string) scenarioResult {
	GinkgoHelper()
	dir, log := newFakeCommands("scenario", "kubectl", "docker")
	env := map[string]string{"E2E_GO_FAKE": "scenario", "PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "SCENARIO_STATE": filepath.Join(dir, "state"), "COMMAND_LOG": log, "SCENARIO_FAILURE": "", "E2E_SCENARIOS": "all", "E2E_SCENARIO_TIMEOUT": "10", "IDENTITY_ONLY": "", "E2E_COMMAND_TIMEOUT": "1", "SCENARIO_SLOW_DELAY": "0.65"}
	for k, v := range overrides {
		env[k] = v
	}
	script := `set -euo pipefail
REPO_ROOT=$PWD
source test/e2e/driver.sh
source test/e2e/target.sh
source test/e2e/scenarios.sh
validate_scenarios
NAMESPACE=owned; POD_NAME=workload; PVC_NAME=claim; TARGET_NAME=target
OWNER_LABEL=owner; E2E_RUN_ID=run; CLEANUP_LABEL=invocation; CLEANUP_TOKEN=unique
E2E_DRIVER_IMAGE=driver:test; E2E_TARGET_IMAGE=target:test
E2E_DRIVER_DELIVERY=registry
if [[ -n "$IDENTITY_ONLY" ]]; then E2E_DRIVER_DELIVERY=kind; fi
DRIVER_EXPECTED_ID=sha256:$(printf 'a%.0s' {1..64})
TARGET_EXPECTED_ID=$DRIVER_EXPECTED_ID; TARGET_HOST=10.42.0.9
EXPECTED_PAYLOAD='iSCSI CSI test successful'
case "$IDENTITY_ONLY" in
    driver) verify_driver ;;
    target) target_endpoint ;;
    *) run_scenarios ;;
esac
`
	result := runCommand(env, "bash", "-c", script)
	return scenarioResult{result, readCalls(log)}
}
func scenarioCalls(calls [][]string, predicate func([]string) bool) [][]string {
	var matched [][]string
	for _, c := range calls {
		if predicate(c) {
			matched = append(matched, c)
		}
	}
	return matched
}
func scenarioCallIndex(calls [][]string, predicate func([]string) bool) int {
	GinkgoHelper()
	for i, c := range calls {
		if predicate(c) {
			return i
		}
	}
	Fail("expected scenario command was not called")
	return -1
}

var _ = Describe("Restart scenarios", func() {
	It("verifies all restart transitions without reseeding", func() {
		r := runScenario(nil)
		Expect(r.Code).To(Equal(0), r.Stderr)
		for _, scenario := range []string{"republish", "driver-restart", "target-restart"} {
			Expect(r.Stdout).To(ContainSubstring("Scenario " + scenario + " data check passed; overall teardown pending"))
		}
		Expect(r.Stdout).NotTo(ContainSubstring("E2E test PASSED"))
		manifest := r.Calls[scenarioCallIndex(r.Calls, func(c []string) bool { return c[0] == "manifest" })][1]
		Expect(manifest).To(ContainSubstring("exec sleep 3600"))
		Expect(manifest).NotTo(ContainSubstring("test-file"))
		reads := scenarioCalls(r.Calls, func(c []string) bool { return c[0] == "exec" && slices.Contains(c, "cat") })
		Expect(reads).To(HaveLen(3))
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "seed") })).To(BeEmpty())
		kill := scenarioCallIndex(r.Calls, func(c []string) bool { return slices.Contains(c, "kill -TERM 1") })
		subsequent := r.Calls[kill+1:]
		Expect(hasCall(subsequent, "get", "pod", "target-test")).To(BeTrue())
		Expect(hasCall(subsequent, "get", "pods")).To(BeTrue())
	})
	It("allows each scenario to be selected independently", func() {
		for _, scenario := range []string{"baseline", "republish", "driver-restart", "target-restart"} {
			By(scenario)
			r := runScenario(map[string]string{"E2E_SCENARIOS": scenario})
			Expect(r.Code).To(Equal(0), r.Stderr)
			if scenario == "baseline" {
				Expect(r.Calls).To(BeEmpty())
			} else {
				Expect(r.Stdout).To(ContainSubstring("Scenario " + scenario + " data check passed"))
			}
		}
	})
	It("fails each corrupted transition without printing the data", func() {
		for _, scenario := range []string{"republish", "driver-restart", "target-restart"} {
			By(scenario)
			r := runScenario(map[string]string{"E2E_SCENARIOS": scenario, "SCENARIO_FAILURE": scenario})
			Expect(r.Code).NotTo(Equal(0))
			Expect(r.Stderr).To(ContainSubstring("unexpected file contents"))
			Expect(r.Stdout + r.Stderr).NotTo(ContainSubstring("CORRUPT-SENTINEL"))
			Expect(r.Stdout).NotTo(ContainSubstring("Scenario " + scenario + " data check passed"))
		}
	})
	It("requires the target restart count to increase", func() {
		r := runScenario(map[string]string{"E2E_SCENARIOS": "target-restart", "SCENARIO_FAILURE": "target-no-restart", "E2E_SCENARIO_TIMEOUT": "1"})
		Expect(r.Code).To(Equal(124), r.Stderr)
		Expect(r.Stdout).NotTo(ContainSubstring("target-restart data check passed"))
	})
	It("rejects a replacement target pod as persistence evidence", func() {
		r := runScenario(map[string]string{"E2E_SCENARIOS": "target-restart", "SCENARIO_FAILURE": "target-pod-change"})
		Expect(r.Code).NotTo(Equal(0))
		Expect(r.Stderr).To(ContainSubstring("Target pod changed"))
	})
	It("requires replacement driver pod identities", func() {
		r := runScenario(map[string]string{"E2E_SCENARIOS": "driver-restart", "SCENARIO_FAILURE": "driver-no-restart"})
		Expect(r.Code).NotTo(Equal(0))
		Expect(r.Stderr).To(ContainSubstring("Driver pods were not replaced"))
	})
	It("bounds kind runtime identity fallback", func() {
		for _, component := range []string{"driver", "target"} {
			By(component)
			r := runScenario(map[string]string{"E2E_SCENARIOS": "baseline", "SCENARIO_FAILURE": component + "-identity-hang", "IDENTITY_ONLY": component})
			Expect(r.Code).NotTo(Equal(0))
			Expect(r.Stderr).To(ContainSubstring("Timed out resolving kind " + component + " image identity"))
			Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "crictl") })).NotTo(BeEmpty())
		}
	})
	It("shares one aggregate deadline across kind identity fallbacks", func() {
		started := time.Now()
		r := runScenario(map[string]string{"E2E_SCENARIOS": "baseline", "SCENARIO_FAILURE": "driver-slow-fallbacks", "IDENTITY_ONLY": "driver"})
		Expect(r.Code).NotTo(Equal(0))
		Expect(r.Stderr).To(ContainSubstring("Timed out resolving kind driver image identity"))
		Expect(time.Since(started)).To(BeNumerically("<", 2500*time.Millisecond))
		Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "crictl") })).To(HaveLen(2))
	})
	It("includes the initial API query in the identity deadline", func() {
		for _, component := range []string{"driver", "target"} {
			By(component)
			started := time.Now()
			r := runScenario(map[string]string{"E2E_SCENARIOS": "baseline", "SCENARIO_FAILURE": component + "-query-hang", "IDENTITY_ONLY": component})
			Expect(r.Code).NotTo(Equal(0))
			Expect(r.Stderr).To(ContainSubstring("Timed out resolving kind " + component + " image identity"))
			Expect(time.Since(started)).To(BeNumerically("<", 2500*time.Millisecond))
			Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "crictl") })).To(BeEmpty())
		}
	})
	It("deducts successful API query time from the fallback budget", func() {
		for _, component := range []string{"driver", "target"} {
			By(component)
			r := runScenario(map[string]string{"E2E_SCENARIOS": "baseline", "SCENARIO_FAILURE": component + "-slow-query", "IDENTITY_ONLY": component})
			Expect(r.Code).NotTo(Equal(0))
			Expect(r.Stderr).To(ContainSubstring("Timed out resolving kind " + component + " image identity"))
			Expect(scenarioCalls(r.Calls, func(c []string) bool { return slices.Contains(c, "crictl") })).To(HaveLen(1))
		}
	})
	It("rejects unknown and empty selections before API calls", func() {
		for _, selection := range []string{"", "bad", "republish,", "republish,,target-restart"} {
			By(selection)
			r := runScenario(map[string]string{"E2E_SCENARIOS": selection})
			Expect(r.Code).NotTo(Equal(0))
			Expect(r.Calls).To(BeEmpty())
		}
	})
})
