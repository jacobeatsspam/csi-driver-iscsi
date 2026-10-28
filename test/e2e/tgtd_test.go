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
	"context"
	"errors"
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

func init() { commandFakes["tgtd"] = fakeTgtdCommand }

func fakeTgtdCommand(args []string) int {
	state := os.Getenv("STATE")
	name := filepath.Base(os.Args[0])
	if err := appendCommand(filepath.Join(state, "commands"), append([]string{name}, args...)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if name == "tgtd" {
		if err := os.WriteFile(filepath.Join(state, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if os.Getenv("TGTD_DIE") != "" {
			return 9
		}
		if os.Getenv("IGNORE_TERM") != "" {
			signal.Ignore(syscall.SIGTERM)
			time.Sleep(20 * time.Second)
			return 0
		}
		stopped := make(chan os.Signal, 1)
		signal.Notify(stopped, syscall.SIGTERM)
		defer signal.Stop(stopped)
		select {
		case <-stopped:
		case <-time.After(20 * time.Second):
		}
		return 0
	}
	argValue := func(key string) string {
		for i, value := range args {
			if value == key && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	mode, op := argValue("--mode"), argValue("--op")
	if (mode == "sys" && os.Getenv("HANG_CONTROL") != "") || (mode == "target" && op == "show" && os.Getenv("HANG_PROBE") != "") {
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(20 * time.Second)
	}
	if mode == "sys" {
		counter := filepath.Join(state, "attempts")
		data, err := os.ReadFile(counter)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		count, _ := strconv.Atoi(string(data))
		count++
		if err := os.WriteFile(counter, []byte(strconv.Itoa(count)), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		delay, _ := strconv.Atoi(os.Getenv("READY_DELAY"))
		if count <= delay {
			return 1
		}
		return 0
	}
	if os.Getenv("FAIL_SETUP") == mode && op == "new" {
		return 7
	}
	if mode == "target" && op == "show" {
		if argValue("--tid") != "" {
			fmt.Println("nop_interval=0\nMaxRecvDataSegmentLength=8192")
			return 0
		}
		if os.Getenv("FAIL_PROBE") != "" {
			return 8
		}
		issue := os.Getenv("PROBE_ISSUE")
		iqn, lun, online, path := os.Getenv("IQN"), "1", "Yes", os.Getenv("DISK_PATH")
		switch issue {
		case "iqn":
			iqn = "iqn.other"
		case "lun":
			lun = "2"
		case "offline":
			online = "No"
		case "path":
			path = "/other/file"
		}
		if issue == "other-target" {
			fmt.Printf("Target 1: %s\n    LUN: 0\n        Online: Yes\n        Backing store path: None\nTarget 2: iqn.other\n    LUN: 1\n        Online: Yes\n        Backing store path: %s\n", iqn, path)
		} else {
			fmt.Printf("Target 1: %s\n    LUN: 0\n        Online: Yes\n        Backing store path: None\n    LUN: %s\n        Online: %s\n        Backing store path: %s\n", iqn, lun, online, path)
		}
		if os.Getenv("LUN_COUNT") == "2" && issue != "missing-second" {
			online = "Yes"
			if issue == "second-offline" {
				online = "No"
			}
			fmt.Printf("    LUN: 2\n        Online: %s\n        Backing store path: %s\n", online, os.Getenv("DISK_PATH_B"))
		}
	}
	return 0
}

func tgtdFixture(extra map[string]string) map[string]string {
	GinkgoHelper()
	dir, _ := newFakeCommands("tgtd", "tgtd", "tgtadm")
	env := map[string]string{
		"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "E2E_GO_FAKE": "tgtd", "STATE": dir,
		"DISK_PATH": filepath.Join(dir, "data", "disk.img"), "DISK_PATH_B": filepath.Join(dir, "data", "disk1.img"), "DISK_SIZE": "1",
		"IQN": "iqn.2026-01.test:fixture", "LUN_COUNT": "1", "TGTD_STARTUP_INTERVAL": "0",
		"TGTD_STARTUP_ATTEMPTS": "4", "TGTD_CONTROL_TIMEOUT": "1", "TGTD_SHUTDOWN_TIMEOUT": "1",
		"TGTD_DIE": "", "IGNORE_TERM": "", "HANG_CONTROL": "", "HANG_PROBE": "", "READY_DELAY": "0", "FAIL_SETUP": "", "FAIL_PROBE": "", "PROBE_ISSUE": "",
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

func runTgtdEntrypoint(env map[string]string) (int, string, bool) {
	GinkgoHelper()
	output := filepath.Join(env["STATE"], "output")
	file, err := os.Create(output)
	Expect(err).NotTo(HaveOccurred())
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(repoRoot, "deploy/example/tgtd/entrypoint.sh"))
	cmd.Env = commandEnvironment(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = file, file
	Expect(cmd.Start()).To(Succeed())
	// Leave absence checks to the spec before emergency group cleanup.
	DeferCleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	configured, finished := false, false
	startup := time.NewTimer(5 * time.Second)
	defer startup.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
startupLoop:
	for {
		data, readErr := os.ReadFile(output)
		Expect(readErr).NotTo(HaveOccurred())
		configured = strings.Contains(string(data), "iSCSI target configured and ready")
		if configured {
			break
		}
		select {
		case <-done:
			finished = true
			break startupLoop
		case <-startup.C:
			break startupLoop
		case <-ticker.C:
		}
	}
	if !finished {
		// Only signal the entrypoint: it must stop and reap its daemon itself.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			Fail("target entrypoint did not stop within its shutdown budget")
		}
	}
	Expect(ctx.Err()).NotTo(HaveOccurred())
	data, err := os.ReadFile(output)
	Expect(err).NotTo(HaveOccurred())
	configured = strings.Contains(string(data), "iSCSI target configured and ready")
	return cmd.ProcessState.ExitCode(), string(data), configured
}

func expectTgtdStopped(directory string) {
	GinkgoHelper()
	data, err := os.ReadFile(filepath.Join(directory, "pid"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
	pid, err := strconv.Atoi(string(data))
	Expect(err).NotTo(HaveOccurred())
	Expect(syscall.Kill(pid, 0)).To(MatchError(syscall.ESRCH), "fake tgtd process %d is still present", pid)
}

func seedTgtdBacking(path string, payload []byte) []byte {
	GinkgoHelper()
	Expect(os.MkdirAll(filepath.Dir(path), 0700)).To(Succeed())
	data := make([]byte, 1024*1024)
	copy(data[4096:], payload)
	Expect(os.WriteFile(path, data, 0600)).To(Succeed())
	return data
}

func readTgtdFile(path string) []byte {
	GinkgoHelper()
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return data
}

var _ = Describe("Target fixture startup and readiness", func() {
	It("retries control readiness before configuring the target", func() {
		env := tgtdFixture(map[string]string{"READY_DELAY": "2"})
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeTrue(), out)
		Expect(code).To(Equal(143), out)
		Expect(string(readTgtdFile(filepath.Join(env["STATE"], "attempts")))).To(Equal("3"))
		info, err := os.Stat(env["DISK_PATH"])
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Size()).To(Equal(int64(1024 * 1024)))
		expectTgtdStopped(env["STATE"])
	})
	DescribeTable("never reports readiness after control or setup failure", func(extra map[string]string, expected int) {
		env := tgtdFixture(extra)
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeFalse(), out)
		Expect(code).To(Equal(expected), out)
		expectTgtdStopped(env["STATE"])
	}, Entry("control never ready", map[string]string{"READY_DELAY": "99"}, 1), Entry("daemon exits", map[string]string{"TGTD_DIE": "1"}, 1), Entry("LUN setup fails", map[string]string{"FAIL_SETUP": "logicalunit"}, 7), Entry("readiness command fails", map[string]string{"FAIL_PROBE": "1"}, 8))
	It("bounds a hanging control command and forcibly stops a stubborn daemon", func() {
		env := tgtdFixture(map[string]string{"HANG_CONTROL": "1", "IGNORE_TERM": "1", "TGTD_STARTUP_ATTEMPTS": "1"})
		started := time.Now()
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeFalse(), out)
		Expect(code).To(Equal(1), out)
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
		expectTgtdStopped(env["STATE"])
	})
	It("bounds a hanging readiness probe", func() {
		env := tgtdFixture(map[string]string{"HANG_PROBE": "1"})
		started := time.Now()
		result := runCommandTimeout(4*time.Second, env, "bash", filepath.Join(repoRoot, "deploy/example/tgtd/ready.sh"))
		Expect(result.Code).NotTo(Equal(0))
		Expect(time.Since(started)).To(BeNumerically("<", 4*time.Second))
	})
	It("kills a configured daemon that ignores TERM during shutdown", func() {
		env := tgtdFixture(map[string]string{"IGNORE_TERM": "1"})
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeTrue(), out)
		Expect(code).To(Equal(143), out)
		expectTgtdStopped(env["STATE"])
	})
	It("preserves existing backing bytes across two starts", func() {
		env := tgtdFixture(nil)
		before := seedTgtdBacking(env["DISK_PATH"], []byte("preserve-this-payload"))
		for range 2 {
			code, out, configured := runTgtdEntrypoint(env)
			Expect(configured).To(BeTrue(), out)
			Expect(code).To(Equal(143), out)
			Expect(readTgtdFile(env["DISK_PATH"])).To(Equal(before))
			expectTgtdStopped(env["STATE"])
		}
	})
	It("requires both LUNs online and preserves both backing files", func() {
		env := tgtdFixture(map[string]string{"LUN_COUNT": "2"})
		before := map[string][]byte{}
		for _, key := range []string{"DISK_PATH", "DISK_PATH_B"} {
			before[key] = seedTgtdBacking(env[key], []byte(env[key]))
		}
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeTrue(), out)
		Expect(code).To(Equal(143), out)
		for _, key := range []string{"DISK_PATH", "DISK_PATH_B"} {
			Expect(readTgtdFile(env[key])).To(Equal(before[key]))
		}
		commands := readCalls(filepath.Join(env["STATE"], "commands"))
		found := false
		for _, call := range commands {
			for index, value := range call {
				if value == "--lun" && index+1 < len(call) && call[index+1] == "2" {
					found = true
				}
			}
		}
		Expect(found).To(BeTrue())
		for _, issue := range []string{"missing-second", "second-offline"} {
			env["PROBE_ISSUE"] = issue
			result := runCommand(env, "bash", filepath.Join(repoRoot, "deploy/example/tgtd/ready.sh"))
			Expect(result.Code).NotTo(Equal(0), issue)
		}
		expectTgtdStopped(env["STATE"])
	})
	DescribeTable("refuses invalid backing paths or sizes without overwriting", func(issue string) {
		env := tgtdFixture(nil)
		backing, sentinel := env["DISK_PATH"], filepath.Join(env["STATE"], "sentinel")
		Expect(os.MkdirAll(filepath.Dir(backing), 0700)).To(Succeed())
		writeFile(sentinel, "untouched", 0600)
		switch issue {
		case "wrong-size":
			writeFile(backing, "untouched", 0600)
		case "symlink":
			Expect(os.Symlink(sentinel, backing)).To(Succeed())
		case "directory":
			Expect(os.Mkdir(backing, 0700)).To(Succeed())
		case "invalid-size":
			env["DISK_SIZE"] = "0"
		}
		code, out, configured := runTgtdEntrypoint(env)
		Expect(configured).To(BeFalse(), out)
		Expect(code).NotTo(Equal(0), out)
		_, err := os.Stat(filepath.Join(env["STATE"], "pid"))
		Expect(errors.Is(err, os.ErrNotExist)).To(BeTrue())
		Expect(string(readTgtdFile(sentinel))).To(Equal("untouched"))
		if issue == "wrong-size" {
			Expect(string(readTgtdFile(backing))).To(Equal("untouched"))
		}
	}, Entry("wrong size", "wrong-size"), Entry("symlink", "symlink"), Entry("directory", "directory"), Entry("invalid size", "invalid-size"))
	DescribeTable("requires a live correctly configured online LUN", func(issue string) {
		env := tgtdFixture(map[string]string{"PROBE_ISSUE": issue})
		if issue == "command" {
			env["FAIL_PROBE"] = "1"
		}
		result := runCommand(env, "bash", filepath.Join(repoRoot, "deploy/example/tgtd/ready.sh"))
		Expect(result.Code == 0).To(Equal(issue == ""), result.Stderr)
	}, Entry("ready", ""), Entry("wrong IQN", "iqn"), Entry("wrong LUN", "lun"), Entry("offline", "offline"), Entry("wrong path", "path"), Entry("command failure", "command"), Entry("LUN belongs to another target", "other-target"))
})
