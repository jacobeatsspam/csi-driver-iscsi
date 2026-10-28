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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var driverDigest = "sha256:" + strings.Repeat("a", 64)

func init() { commandFakes["driver"] = fakeDriverCommand }

func fakeDriverCommand(args []string) int {
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
		return 1
	}
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	if os.Getenv("FAIL_COMMAND") == name+" "+first {
		return 9
	}
	switch {
	case name == "git":
		fmt.Println(strings.Repeat("b", 40))
	case name == "docker" && hasCall([][]string{args}, "image", "inspect"):
		if strings.Contains(strings.Join(args, " "), "RepoDigests") {
			fmt.Println(os.Getenv("REPO_DIGESTS"))
		} else {
			fmt.Println(driverDigest)
		}
	case name == "kubectl" && hasCall([][]string{args}, "get", "pods"):
		image := "iscsi-e2e:test"
		if os.Getenv("DELIVERY") == "registry" {
			image = "registry.example/iscsi@" + driverDigest
		}
		runtime := driverDigest
		if os.Getenv("MISMATCH") != "" || os.Getenv("MANIFEST_ID") != "" {
			runtime = "sha256:" + strings.Repeat("c", 64)
		}
		payload := map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "driver-test"},
			"spec":     map[string]any{"nodeName": "test-worker", "containers": []any{map[string]any{"name": "iscsi", "image": image}}},
			"status":   map[string]any{"containerStatuses": []any{map[string]any{"name": "iscsi", "ready": true, "imageID": runtime}}},
		}}}
		_ = json.NewEncoder(os.Stdout).Encode(payload)
	case name == "kubectl" && hasCall([][]string{args}, "config", "current-context"):
		fmt.Println(os.Getenv("TEST_CONTEXT"))
	case name == "docker" && first == "exec":
		digest := driverDigest
		if os.Getenv("MISMATCH") != "" {
			digest = "sha256:" + strings.Repeat("c", 64)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": map[string]string{"id": digest}})
	case name == "kubectl" && first == "apply":
		data, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := appendCommand(os.Getenv("COMMAND_LOG"), []string{"manifest", string(data)}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

type driverOptions struct {
	failure, delivery, context string
	mismatch, manifestID       bool
	repoDigests                []string
}

func runDriver(options driverOptions) (commandResult, [][]string) {
	GinkgoHelper()
	if options.delivery == "" {
		options.delivery = "kind"
	}
	if options.context == "" {
		options.context = "kind-test"
	}
	if options.repoDigests == nil {
		options.repoDigests = []string{"registry.example/iscsi@" + driverDigest}
	}
	dir, log := newFakeCommands("driver", "git", "docker", "make", "kind", "kubectl")
	digests, err := json.Marshal(options.repoDigests)
	Expect(err).NotTo(HaveOccurred())
	image := "iscsi-e2e:test"
	if options.delivery == "registry" {
		image = "registry.example/iscsi:test"
	}
	env := map[string]string{
		"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "E2E_GO_FAKE": "driver", "COMMAND_LOG": log,
		"FAIL_COMMAND": options.failure, "E2E_DRIVER_IMAGE": image, "E2E_KIND_CLUSTER": "test", "E2E_ARCH": "amd64",
		"E2E_DRIVER_DELIVERY": options.delivery, "DELIVERY": options.delivery, "MISMATCH": "", "MANIFEST_ID": "",
		"TEST_CONTEXT": options.context, "REPO_DIGESTS": string(digests),
		"TMPDIR": dir,
	}
	if options.mismatch {
		env["MISMATCH"] = "yes"
	}
	if options.manifestID {
		env["MANIFEST_ID"] = "yes"
	}
	// The && deliberately disables implicit errexit in functions: explicit
	// failure propagation must still work.
	result := runCommand(env, "bash", "-c", `set -euo pipefail; REPO_ROOT=$1; source "$REPO_ROOT/test/e2e/driver.sh"; prepare_driver && install_driver`, "test", repoRoot)
	return result, readCalls(log)
}

func driverManifests(calls [][]string) string {
	var result []string
	for _, call := range calls {
		if len(call) > 1 && call[0] == "manifest" {
			result = append(result, call[1])
		}
	}
	return strings.Join(result, "\n")
}

var _ = Describe("Local driver installation", func() {
	It("loads the checkout build into kind and applies its manifests", func() {
		result, calls := runDriver(driverOptions{})
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(calls).To(ContainElement([]string{"kind", "load", "docker-image", "iscsi-e2e:test", "--name", "test"}))
		manifests := driverManifests(calls)
		Expect(manifests).To(ContainSubstring("image: iscsi-e2e:test"))
		Expect(manifests).To(ContainSubstring("imagePullPolicy: Never"))
		Expect(manifests).NotTo(ContainSubstring("iscsiplugin:canary"))
		Expect(result.Stdout).To(ContainSubstring("Verified driver pod"))
		Expect(result.Stdout).To(ContainSubstring(strings.Repeat("b", 40)))
	})
	It("uses the pushed registry digest", func() {
		result, calls := runDriver(driverOptions{delivery: "registry"})
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(calls).To(ContainElement([]string{"docker", "push", "registry.example/iscsi:test"}))
		Expect(driverManifests(calls)).To(ContainSubstring("image: registry.example/iscsi@" + driverDigest))
	})
	It("selects the matching repository rather than the first digest", func() {
		result, calls := runDriver(driverOptions{delivery: "registry", repoDigests: []string{"other.example/foreign@sha256:" + strings.Repeat("d", 64), "registry.example/iscsi@" + driverDigest}})
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(driverManifests(calls)).To(ContainSubstring("image: registry.example/iscsi@" + driverDigest))
	})
	DescribeTable("rejects missing or ambiguous matching registry digests", func(digests []string) {
		result, calls := runDriver(driverOptions{delivery: "registry", repoDigests: digests})
		Expect(result.Code).NotTo(Equal(0))
		Expect(result.Stderr).To(ContainSubstring("unique pushed image digest"))
		Expect(hasCall(calls, "kubectl")).To(BeFalse())
	}, Entry("empty list", []string{}), Entry("foreign repository", []string{"other.example/foreign@" + driverDigest}), Entry("ambiguous digest", []string{"registry.example/iscsi@" + driverDigest, "registry.example/iscsi@sha256:" + strings.Repeat("d", 64)}))
	DescribeTable("stops build or delivery failures before cluster mutation", func(failure string) {
		delivery := "kind"
		if failure == "docker push" {
			delivery = "registry"
		}
		result, calls := runDriver(driverOptions{failure: failure, delivery: delivery})
		Expect(result.Code).NotTo(Equal(0))
		for _, call := range calls {
			if call[0] == "kubectl" {
				Expect(call[1:]).To(Equal([]string{"config", "current-context"}))
			}
		}
	}, Entry("driver binary", "make -C"), Entry("container build", "docker build"), Entry("kind delivery", "kind load"), Entry("registry delivery", "docker push"))
	It("propagates rollout failure", func() {
		result, calls := runDriver(driverOptions{failure: "kubectl rollout"})
		Expect(result.Code).NotTo(Equal(0))
		Expect(hasCall(calls, "kubectl", "get", "pods")).To(BeFalse())
	})
	It("rejects a running image that differs from the selected image", func() {
		result, _ := runDriver(driverOptions{mismatch: true})
		Expect(result.Code).NotTo(Equal(0))
		Expect(result.Stderr).To(ContainSubstring("does not run the selected"))
	})
	It("resolves a kind runtime manifest to the local image config", func() {
		result, calls := runDriver(driverOptions{manifestID: true})
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(hasCall(calls, "docker", "exec", "test-worker", "crictl")).To(BeTrue())
	})
	It("rejects the wrong kind context before building or mutation", func() {
		result, calls := runDriver(driverOptions{context: "production"})
		Expect(result.Code).NotTo(Equal(0))
		for _, name := range []string{"make", "docker", "kind"} {
			Expect(hasCall(calls, name)).To(BeFalse())
		}
		Expect(result.Stderr).To(ContainSubstring("does not match"))
	})
})
