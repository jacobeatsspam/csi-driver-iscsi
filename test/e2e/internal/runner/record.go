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

package runner

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var phasePattern = regexp.MustCompile(`^Scenario (baseline|republish|driver-restart|target-restart|shared-target) data check passed; overall teardown pending$`)
var digestPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
var imagePattern = regexp.MustCompile(`^(Driver|Target) image: \S+; local ID: sha256:[0-9a-f]{64}(; expected runtime ID: sha256:[0-9a-f]{64})?$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Record runs the selected command and writes only allowlisted evidence.
func Record(args []string) int {
	flags := flag.NewFlagSet("record-run", flag.ContinueOnError)
	artifacts := flags.String("artifacts", "", "new directory for summary.json")
	budget := flags.Int("timeout", 1800, "whole-command budget in seconds")
	if flags.Parse(args) != nil {
		return 2
	}
	if *artifacts == "" || *budget < 1 {
		fmt.Fprintln(os.Stderr, "--artifacts is required and --timeout must be positive")
		return 2
	}
	root, err := repositoryRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err = os.MkdirAll(filepath.Dir(*artifacts), 0700); err == nil {
		err = os.Mkdir(*artifacts, 0700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "--artifacts must be a new writable directory")
		return 2
	}
	command := flags.Args()
	custom := len(command) > 0
	if !custom {
		command = []string{"bash", filepath.Join(root, "test/e2e/run-test.sh")}
	}
	selected, ok := os.LookupEnv("E2E_SCENARIOS")
	if !ok {
		selected = "baseline"
	}
	allowed := map[string]bool{}
	for _, s := range []string{"baseline", "republish", "driver-restart", "target-restart", "shared-target", "all", "chap-secret-ref", "stats", "raw-block", "expansion", "multipath"} {
		allowed[s] = true
	}
	for _, s := range strings.Split(selected, ",") {
		if !allowed[s] {
			selected = "invalid"
			break
		}
	}
	kind := "harness"
	if custom {
		kind = "custom"
	}
	summary := map[string]any{
		"started_utc": time.Now().UTC().Format(time.RFC3339Nano), "command_kind": kind,
		"scenarios": selected, "exit_code": nil, "data_phases_passed": []string{},
		"image_digests": []string{}, "overall": "failed", "nodes": []map[string]any{}, "source_dirty": nil,
		"source_revision": "unavailable",
	}
	if revision, err := capture(10*time.Second, "git", "-C", root, "rev-parse", "HEAD"); err == nil {
		rev := strings.TrimSpace(string(revision))
		if revisionPattern.MatchString(rev) {
			summary["source_revision"] = rev
		}
		if status, err := capture(10*time.Second, "git", "-C", root, "status", "--porcelain"); err == nil {
			summary["source_dirty"] = strings.TrimSpace(string(status)) != ""
		}
	}
	code := 127
	raw, err := os.CreateTemp("", "iscsi-record-output-*")
	if err == nil {
		defer raw.Close()
		if err = os.Remove(raw.Name()); err == nil {
			cmd := exec.Command(command[0], command[1:]...)
			cmd.Dir, cmd.Stdout, cmd.Stderr = root, raw, raw
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			interrupts := make(chan os.Signal, 2)
			signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
			if err = cmd.Start(); err == nil {
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				timer := time.NewTimer(time.Duration(*budget) * time.Second)
				select {
				case err := <-done:
					code = exitCode(err)
					summary["command_exit_code"] = code
					if groupExists(cmd.Process.Pid) {
						stopGroup(cmd.Process.Pid)
						summary["background_processes_cleaned"] = true
						if code == 0 {
							code = 1
						}
					}
				case <-timer.C:
					stopGroup(cmd.Process.Pid)
					<-done
					code = 124
				case sig := <-interrupts:
					stopGroup(cmd.Process.Pid)
					<-done
					code = 128 + int(sig.(syscall.Signal))
				}
				timer.Stop()
			} else {
				fmt.Fprintln(os.Stderr, "Unable to start recorded command")
			}
			signal.Stop(interrupts)
			if _, err := raw.Seek(0, io.SeekStart); err == nil {
				reader := bufio.NewReader(raw)
				phases, digests := []string{}, map[string]bool{}
				for {
					line, err := reader.ReadString('\n')
					line = strings.TrimSuffix(line, "\n")
					if match := phasePattern.FindStringSubmatch(line); match != nil {
						phases = append(phases, match[1])
						fmt.Println(line)
					}
					if imagePattern.MatchString(line) {
						for _, d := range digestPattern.FindAllString(line, -1) {
							digests[d] = true
						}
					}
					if err != nil {
						break
					}
				}
				values := []string{}
				for d := range digests {
					values = append(values, d)
				}
				sort.Strings(values)
				summary["data_phases_passed"], summary["image_digests"] = phases, values
			}
		} else {
			fmt.Fprintln(os.Stderr, "Unable to create private command output")
		}
	} else {
		fmt.Fprintln(os.Stderr, "Unable to create private command output")
	}
	summary["exit_code"] = code
	if code == 0 {
		summary["overall"] = "passed"
	} else if code == 78 {
		summary["overall"] = "gated"
	}
	if !custom && code != 2 && code != 78 && code != 127 {
		nodes, err := nodeMetadata()
		if err != nil {
			summary["node_metadata"] = "unavailable"
		} else {
			summary["nodes"] = nodes
		}
	}
	summary["finished_utc"] = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(*artifacts, "summary.json"), append(encoded, '\n'), 0600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to write evidence; original command exit was %d\n", code)
		if code == 0 {
			return 1
		}
		return code
	}
	fmt.Printf("Recorded command exited %d; overall %s\n", code, summary["overall"])
	return code
}

func groupExists(pid int) bool { return syscall.Kill(-pid, 0) == nil }

func stopGroup(pid int) {
	if syscall.Kill(-pid, syscall.SIGTERM) == syscall.ESRCH {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !groupExists(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func nodeMetadata() ([]map[string]any, error) {
	payload, err := capture(15*time.Second, "kubectl", "--request-timeout=10s", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if raw, ok := data["items"]; !ok || string(raw) == "null" {
		return nil, fmt.Errorf("invalid node metadata")
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	nodes := []map[string]any{}
	for _, item := range items {
		if item == nil {
			return nil, fmt.Errorf("invalid node metadata")
		}
		var status struct {
			NodeInfo map[string]any `json:"nodeInfo"`
		}
		if raw, ok := item["status"]; ok {
			if err := json.Unmarshal(raw, &status); err != nil {
				return nil, err
			}
		}
		node := map[string]any{}
		for _, key := range []string{"osImage", "kernelVersion", "kubeletVersion", "containerRuntimeVersion"} {
			value, ok := status.NodeInfo[key]
			if !ok {
				value = ""
			}
			node[key] = value
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}
