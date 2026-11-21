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

package localact

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalActRequiresDisposableVM(t *testing.T) {
	for _, tc := range []struct {
		name, disposableVM, virt, actExit string
		wantExit                          int
		wantAct                           bool
	}{
		{"no opt in", "", "0", "0", 2, false},
		{"not a VM", "1", "1", "0", 2, false},
		{"run workflow", "1", "0", "0", 0, true},
		{"propagate failure", "1", "0", "7", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "act.log")
			for name, script := range map[string]string{
				"systemd-detect-virt": "#!/bin/sh\nexit \"$FAKE_VIRT\"\n",
				"act":                 "#!/bin/sh\nprintf '%s\\n' \"$DOCKER_CONTEXT\" \"$DOCKER_HOST\" \"$@\" > \"$FAKE_LOG\"\nexit \"$FAKE_ACT_EXIT\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "../local-act-in-vm.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_DISPOSABLE_VM="+tc.disposableVM,
				"FAKE_VIRT="+tc.virt, "FAKE_ACT_EXIT="+tc.actExit, "FAKE_LOG="+log,
				"DOCKER_CONTEXT=remote", "DOCKER_HOST=tcp://remote:2375")
			output, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					exit = ee.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if exit != tc.wantExit {
				t.Fatalf("exit %d, want %d: %s", exit, tc.wantExit, output)
			}
			data, readErr := os.ReadFile(log)
			if !tc.wantAct {
				if !os.IsNotExist(readErr) {
					t.Fatalf("act ran unexpectedly: %s", data)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "default\nunix:///var/run/docker.sock\npull_request\n-W\n.github/workflows/ubuntu-e2e.yml\n-j\ne2e\n-P\nubuntu-24.04=-self-hosted\n"
			if strings.TrimSpace(string(data)) != strings.TrimSpace(want) {
				t.Fatalf("unexpected act invocation: %s", data)
			}
		})
	}
}

func TestLocalActRejectsUnexpectedVMImage(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "image")
	archive := filepath.Join(dir, "act")
	if err := os.WriteFile(image, []byte("not the pinned image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("not act"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../local-act-e2e.sh")
	cmd.Env = append(os.Environ(), "LOCAL_ACT_VM_IMAGE="+image, "LOCAL_ACT_BINARY_ARCHIVE="+archive, "TMPDIR="+dir)
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "FAILED") {
		t.Fatalf("unexpected image was not rejected: %s, %v", output, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary VM files were not removed: %v, %v", entries, err)
	}
}

func TestLocalActCachesVerifiedVMImage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	tmp := filepath.Join(dir, "tmp")
	for _, path := range []string{bin, tmp} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "downloads")
	archive := filepath.Join(dir, "invalid-act")
	if err := os.WriteFile(archive, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"curl":      "#!/bin/sh\nwhile [ \"$1\" != -o ]; do shift; done\nprintf 'download\\n' >> \"$DOWNLOAD_LOG\"\nprintf image > \"$2\"\n[ \"${FAIL_DOWNLOAD:-0}\" != 1 ]\n",
		"sha256sum": "#!/bin/sh\nread -r hash path\nif [ \"$(cat \"$path\")\" = image ]; then exit 0; fi\necho \"$path: FAILED\"\nexit 1\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(failDownload bool) string {
		t.Helper()
		cmd := exec.Command("bash", "../local-act-e2e.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp,
			"LOCAL_ACT_VM_IMAGE=", "LOCAL_ACT_BINARY_ARCHIVE="+archive, "DOWNLOAD_LOG="+log, "FAIL_DOWNLOAD=0")
		if failDownload {
			cmd.Env = append(cmd.Env, "FAIL_DOWNLOAD=1")
		}
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected download or act checksum failure: %s", output)
		}
		return string(output)
	}
	cache := filepath.Join(tmp, "csi-local-act-ubuntu-noble-20261017-"+strconv.Itoa(os.Getuid())+".img")
	run(false)
	if data, err := os.ReadFile(cache); err != nil || string(data) != "image" {
		t.Fatalf("verified image was not cached: %q, %v", data, err)
	}
	run(false)
	if data, err := os.ReadFile(log); err != nil || strings.Count(string(data), "download\n") != 1 {
		t.Fatalf("cached image was downloaded again: %q, %v", data, err)
	}
	if err := os.WriteFile(cache, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	run(false)
	if data, err := os.ReadFile(log); err != nil || strings.Count(string(data), "download\n") != 2 {
		t.Fatalf("corrupt image was not replaced: %q, %v", data, err)
	}
	if data, err := os.ReadFile(cache); err != nil || string(data) != "image" {
		t.Fatalf("replacement image is invalid: %q, %v", data, err)
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	run(true)
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("partial download was cached: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary VM files were not removed: %v, %v", entries, err)
	}
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, cache); err != nil {
		t.Fatal(err)
	}
	output := run(false)
	if !strings.Contains(output, "Refusing unsafe cached VM image") {
		t.Fatalf("unsafe cache path was not rejected: %s", output)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
		t.Fatalf("unsafe cache path changed its target: %q, %v", data, err)
	}
}

func TestLocalActStopsVMOnInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		signal     syscall.Signal
		exit       int
		ignoreTerm bool
	}{
		{"interrupt", syscall.SIGINT, 130, false},
		{"terminate", syscall.SIGTERM, 143, false},
		{"hangup", syscall.SIGHUP, 129, false},
		{"unresponsive VM", syscall.SIGTERM, 143, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			tmp := filepath.Join(dir, "tmp")
			for _, path := range []string{bin, tmp} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name, script := range map[string]string{
				"sha256sum":          "#!/bin/sh\ncat >/dev/null\n",
				"git":                "#!/bin/sh\nexit 0\n",
				"xorriso":            "#!/bin/sh\nexit 0\n",
				"qemu-img":           "#!/bin/sh\nexit 0\n",
				"qemu-system-x86_64": "#!/bin/sh\n[ \"$IGNORE_TERM\" != 1 ] || trap '' TERM\nprintf '%s' \"$$\" > \"$FAKE_VM_PID\"\nexec sleep 300\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			image, archive := filepath.Join(dir, "image"), filepath.Join(dir, "act")
			for _, path := range []string{image, archive} {
				if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pidFile := filepath.Join(dir, "vm.pid")
			cmd := exec.Command("bash", "../local-act-e2e.sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp,
				"LOCAL_ACT_VM_IMAGE="+image, "LOCAL_ACT_BINARY_ARCHIVE="+archive, "FAKE_VM_PID="+pidFile,
				"IGNORE_TERM="+strconv.FormatBool(tc.ignoreTerm))
			if tc.ignoreTerm {
				cmd.Env = append(cmd.Env, "IGNORE_TERM=1")
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			var vmPID int
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if data, err := os.ReadFile(pidFile); err == nil {
					vmPID, _ = strconv.Atoi(string(data))
					if vmPID > 0 {
						break
					}
				}
			}
			if vmPID == 0 {
				t.Fatal("VM did not start")
			}
			vmStopped := false
			t.Cleanup(func() {
				if !vmStopped {
					_ = syscall.Kill(vmPID, syscall.SIGKILL)
				}
			})
			if err := cmd.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != tc.exit {
					t.Fatalf("exit after %s: %v", tc.signal, err)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("interrupted VM did not stop")
			}
			if err := syscall.Kill(vmPID, 0); err == nil {
				stat, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(vmPID), "stat"))
				if !strings.Contains(string(stat), ") Z ") {
					t.Fatalf("VM process %d is still running", vmPID)
				}
			}
			vmStopped = true
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary VM files were not removed: %v, %v", entries, err)
			}
			if _, err := os.Stat(image); err != nil {
				t.Fatalf("base image was removed: %v", err)
			}
		})
	}
}

func TestInstallKubectlRejectsUnexpectedDownload(t *testing.T) {
	dir := t.TempDir()
	curl := "#!/bin/sh\nwhile [ \"$1\" != -o ]; do shift; done\nprintf invalid > \"$2\"\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0755); err != nil {
		t.Fatal(err)
	}
	pathFile := filepath.Join(dir, "github-path")
	cmd := exec.Command("bash", "../install-kubectl.sh")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "RUNNER_TEMP="+dir, "GITHUB_PATH="+pathFile)
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "FAILED") {
		t.Fatalf("unexpected kubectl binary was not rejected: %s, %v", output, err)
	}
	if _, err := os.Stat(pathFile); !os.IsNotExist(err) {
		t.Fatalf("invalid binary added to PATH: %v", err)
	}
}
