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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() { commandFakes["record-test"] = recordFake }

func recordFake(args []string) int {
	switch filepath.Base(os.Args[0]) {
	case "git":
		if os.Getenv("RECORD_GIT") == "unknown" {
			return 7
		}
		for _, arg := range args {
			if arg == "rev-parse" {
				fmt.Println(strings.Repeat("a", 40))
				return 0
			}
			if arg == "status" {
				if os.Getenv("RECORD_GIT") == "dirty" {
					fmt.Println(" M secret-sentinel")
				}
				return 0
			}
		}
	case "kubectl":
		fmt.Print(`{"items":[{"metadata":{"annotations":{"secret":"secret-sentinel"}},"status":{"nodeInfo":{"osImage":"Test Linux","kernelVersion":"1","kubeletVersion":"v1","containerRuntimeVersion":"test"}}}]}`)
	case "bash":
		fmt.Println("secret-sentinel")
		return 17
	case "record-child":
		switch args[0] {
		case "output":
			fmt.Println(args[2])
			delay, _ := strconv.ParseFloat(args[3], 64)
			time.Sleep(time.Duration(delay * float64(time.Second)))
			code, _ := strconv.Atoi(args[1])
			return code
		case "write-failure":
			if err := os.Mkdir(args[2], 0700); err != nil {
				return 90
			}
			code, _ := strconv.Atoi(args[1])
			return code
		case "started":
			if err := os.WriteFile(args[1], nil, 0600); err != nil {
				return 90
			}
			time.Sleep(30 * time.Second)
		case "descendant":
			signal.Ignore(syscall.SIGTERM)
			if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				return 90
			}
			time.Sleep(30 * time.Second)
		case "leader":
			child := exec.Command(os.Args[0], "descendant", args[1])
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				return 90
			}
			if args[2] == "success" {
				for i := 0; i < 250; i++ {
					if _, err := os.Stat(args[1]); err == nil {
						return 0
					}
					time.Sleep(20 * time.Millisecond)
				}
				return 91
			}
			time.Sleep(30 * time.Second)
		}
	}
	return 0
}

func recordSummary(path string) map[string]any {
	GinkgoHelper()
	data, err := os.ReadFile(filepath.Join(path, "summary.json"))
	Expect(err).NotTo(HaveOccurred())
	var summary map[string]any
	Expect(json.Unmarshal(data, &summary)).To(Succeed())
	return summary
}

func recordCombined(result commandResult, summary map[string]any) string {
	data, _ := json.Marshal(summary)
	return string(data) + result.Stdout + result.Stderr
}

var _ = Describe("Recorded E2E evidence", Ordered, func() {
	var binary string
	BeforeAll(func() {
		dir, err := os.MkdirTemp("", "iscsi-record-run-tests-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		binary = filepath.Join(dir, "record-run")
		result := runCommandTimeout(2*time.Minute, nil, "go", "-C", filepath.Join(repoRoot, "test/e2e"), "build", "-buildvcs=false", "-mod=readonly", "-o", binary, "./cmd/record-run")
		Expect(result.Code).To(BeZero(), result.Stderr)
	})
	environment := func(dir string) map[string]string {
		return map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "E2E_GO_FAKE": "record-test", "E2E_SCENARIOS": "baseline"}
	}
	runRecord := func(code int, output, scenario string, budget int, delay string) (commandResult, map[string]any, []string, time.Duration) {
		dir, _ := newFakeCommands("record-test", "record-child", "kubectl", "git")
		env := environment(dir)
		env["E2E_SCENARIOS"] = scenario
		results := filepath.Join(dir, "results")
		started := time.Now()
		result := runCommand(env, binary, "--artifacts", results, "--timeout", strconv.Itoa(budget), "--", filepath.Join(dir, "record-child"), "output", strconv.Itoa(code), output, delay)
		elapsed := time.Since(started)
		entries, err := os.ReadDir(results)
		Expect(err).NotTo(HaveOccurred())
		files := []string{}
		for _, entry := range entries {
			files = append(files, entry.Name())
		}
		return result, recordSummary(results), files, elapsed
	}
	It("preserves a failure after data success and excludes secret output", func() {
		marker := "Scenario baseline data check passed; overall teardown pending"
		result, summary, files, _ := runRecord(17, marker+"\nsecret-sentinel\n"+marker+" secret-sentinel\nDriver image: secret-sentinel", "baseline", 5, "0")
		Expect(result.Code).To(Equal(17))
		Expect(summary["overall"]).To(Equal("failed"))
		Expect(summary["exit_code"]).To(Equal(float64(17)))
		Expect(summary["data_phases_passed"]).To(Equal([]any{"baseline"}))
		Expect(files).To(Equal([]string{"summary.json"}))
		Expect(recordCombined(result, summary)).NotTo(ContainSubstring("secret-sentinel"))
		Expect(recordCombined(result, summary)).NotTo(ContainSubstring("annotations"))
	})
	It("reports success only when its child succeeds", func() {
		result, summary, _, _ := runRecord(0, "", "baseline", 5, "0")
		Expect(result.Code).To(BeZero())
		Expect(summary["overall"]).To(Equal("passed"))
	})
	It("records only allowlisted node metadata for the default harness", func() {
		dir, _ := newFakeCommands("record-test", "bash", "kubectl", "git")
		results := filepath.Join(dir, "results")
		result := runCommand(environment(dir), binary, "--artifacts", results)
		summary := recordSummary(results)
		Expect(result.Code).To(Equal(17))
		Expect(summary["command_kind"]).To(Equal("harness"))
		nodes := summary["nodes"].([]any)
		Expect(nodes[0].(map[string]any)["osImage"]).To(Equal("Test Linux"))
		Expect(recordCombined(result, summary)).NotTo(ContainSubstring("secret-sentinel"))
	})
	It("keeps input errors and feature gates distinct from success without recording raw selections", func() {
		for _, code := range []int{2, 78} {
			result, summary, _, _ := runRecord(code, "", "secret-sentinel", 5, "0")
			Expect(result.Code).To(Equal(code))
			Expect(summary["nodes"]).To(BeEmpty())
			Expect(summary["scenarios"]).To(Equal("invalid"))
			Expect(recordCombined(result, summary)).NotTo(ContainSubstring("secret-sentinel"))
			Expect(summary["overall"]).NotTo(Equal("passed"))
		}
	})
	It("writes failure evidence after a whole-command timeout", func() {
		result, summary, _, elapsed := runRecord(0, "", "baseline", 1, "20")
		Expect(result.Code).To(Equal(124))
		Expect(summary["exit_code"]).To(Equal(float64(124)))
		Expect(elapsed).To(BeNumerically("<", 5*time.Second))
	})
	for _, success := range []bool{false, true} {
		success := success
		name := "kills a TERM-ignoring descendant when its leader exits during timeout cleanup"
		if success {
			name = "fails a successful leader that leaves background test processes"
		}
		It(name, func() {
			dir, _ := newFakeCommands("record-test", "record-child", "git")
			pidFile, results := filepath.Join(dir, "descendant.pid"), filepath.Join(dir, "results")
			DeferCleanup(func() {
				if data, err := os.ReadFile(pidFile); err == nil {
					pid, _ := strconv.Atoi(string(data))
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			mode, budget, expected := "timeout", "1", 124
			if success {
				mode, budget, expected = "success", "10", 1
			}
			result := runCommandTimeout(8*time.Second, environment(dir), binary, "--artifacts", results, "--timeout", budget, "--", filepath.Join(dir, "record-child"), "leader", pidFile, mode)
			Expect(result.Code).To(Equal(expected), result.Stderr)
			data, err := os.ReadFile(pidFile)
			Expect(err).NotTo(HaveOccurred(), "descendant must start before cleanup")
			pid, err := strconv.Atoi(string(data))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				if os.IsNotExist(err) {
					return true
				}
				if err != nil {
					return false
				}
				fields := strings.Fields(string(data))
				return len(fields) > 2 && (fields[2] == "Z" || fields[2] == "X")
			}, 2*time.Second, 20*time.Millisecond).Should(BeTrue(), "background process survived cleanup")
			summary := recordSummary(results)
			Expect(summary["exit_code"]).To(Equal(float64(expected)))
			if success {
				Expect(summary["command_exit_code"]).To(Equal(float64(0)))
				Expect(summary["background_processes_cleaned"]).To(BeTrue())
			}
		})
	}
	It("never overwrites an existing artifact directory", func() {
		dir, _ := newFakeCommands("record-test", "record-child")
		marker := filepath.Join(dir, "keep")
		writeFile(marker, "original", 0600)
		result := runCommand(environment(dir), binary, "--artifacts", dir, "--", filepath.Join(dir, "record-child"), "output", "0", "", "0")
		Expect(result.Code).NotTo(BeZero())
		data, err := os.ReadFile(marker)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("original"))
		_, err = os.Stat(filepath.Join(dir, "summary.json"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
	It("records dirty, clean, or unknown source state without raw paths", func() {
		for _, mode := range []string{"dirty", "clean", "unknown"} {
			dir, _ := newFakeCommands("record-test", "record-child", "git")
			env := environment(dir)
			env["RECORD_GIT"] = mode
			results := filepath.Join(dir, "results")
			result := runCommand(env, binary, "--artifacts", results, "--", filepath.Join(dir, "record-child"), "output", "0", "", "0")
			summary := recordSummary(results)
			Expect(result.Code).To(BeZero(), result.Stderr)
			switch mode {
			case "dirty":
				Expect(summary["source_dirty"]).To(BeTrue())
			case "clean":
				Expect(summary["source_dirty"]).To(BeFalse())
			case "unknown":
				Expect(summary["source_dirty"]).To(BeNil())
			}
			Expect(recordCombined(result, summary)).NotTo(ContainSubstring("secret-sentinel"))
		}
	})
	It("preserves an original failure if writing the artifact fails", func() {
		for _, original := range []int{17, 0} {
			dir, _ := newFakeCommands("record-test", "record-child", "git")
			results := filepath.Join(dir, "results")
			result := runCommand(environment(dir), binary, "--artifacts", results, "--", filepath.Join(dir, "record-child"), "write-failure", strconv.Itoa(original), filepath.Join(results, "summary.json"))
			expected := original
			if original == 0 {
				expected = 1
			}
			Expect(result.Code).To(Equal(expected))
			Expect(result.Stderr).To(ContainSubstring("Unable to write evidence"))
		}
	})
	It("writes the signal status when interrupted", func() {
		dir, _ := newFakeCommands("record-test", "record-child", "git")
		results, started := filepath.Join(dir, "results"), filepath.Join(dir, "started")
		cmd := exec.Command(binary, "--artifacts", results, "--", filepath.Join(dir, "record-child"), "started", started)
		cmd.Dir, cmd.Env = repoRoot, commandEnvironment(environment(dir))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		Expect(cmd.Start()).To(Succeed())
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		DeferCleanup(func() { _ = cmd.Process.Kill() })
		Eventually(func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, 20*time.Millisecond).Should(BeTrue())
		Expect(cmd.Process.Signal(syscall.SIGTERM)).To(Succeed())
		Eventually(done, 5*time.Second).Should(Receive(HaveOccurred()))
		Expect(cmd.ProcessState.ExitCode()).To(Equal(143), stderr.String())
		Expect(recordSummary(results)["exit_code"]).To(Equal(float64(143)))
	})
})
