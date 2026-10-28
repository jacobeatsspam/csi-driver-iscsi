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

func init() { commandFakes["target"] = fakeTargetCommand }
func fakeTargetCommand(args []string) int {
	name := filepath.Base(os.Args[0])
	if err := appendCommand(os.Getenv("COMMAND_LOG"), append([]string{name}, args...)); err != nil {
		return 1
	}
	if len(args) > 0 && os.Getenv("FAILURE") == name+" "+args[0] {
		return 19
	}
	if name == "docker" && hasCall([][]string{args}, "image", "inspect") {
		if len(args) > 3 && strings.Contains(args[3], "RepoDigests") {
			fmt.Println(os.Getenv("DIGESTS"))
		} else {
			fmt.Println(driverDigest)
		}
	}
	return 0
}

func runTarget(delivery, image, failure string, digests []string) (commandResult, [][]string) {
	GinkgoHelper()
	dir, log := newFakeCommands("target", "docker", "kind")
	if digests == nil {
		digests = []string{"registry.example/target@" + driverDigest}
	}
	data, err := json.Marshal(digests)
	Expect(err).NotTo(HaveOccurred())
	env := map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "E2E_GO_FAKE": "target", "COMMAND_LOG": log, "FAILURE": failure,
		"E2E_DRIVER_DELIVERY": delivery, "E2E_KIND_CLUSTER": "test", "E2E_TARGET_IMAGE": image, "DRIVER_SOURCE_REVISION": strings.Repeat("b", 40), "DIGESTS": string(data)}
	result := runCommand(env, "bash", "-euo", "pipefail", "-c", `REPO_ROOT=$1; source "$1/test/e2e/images.sh"; source "$1/test/e2e/target.sh"; prepare_target; printf 'selected=%s policy=%s expected=%s\n' "$E2E_TARGET_IMAGE" "$TARGET_PULL_POLICY" "$TARGET_EXPECTED_ID"`, "test", repoRoot)
	return result, readCalls(log)
}

var _ = Describe("Target image delivery", func() {
	It("builds the checkout and loads its explicit image into kind", func() {
		result, calls := runTarget("kind", "iscsi-target:test", "", nil)
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(calls).To(ContainElement([]string{"docker", "build", "-t", "iscsi-target:test", filepath.Join(repoRoot, "deploy/example/tgtd")}))
		Expect(calls).To(ContainElement([]string{"kind", "load", "docker-image", "iscsi-target:test", "--name", "test"}))
		Expect(result.Stdout).To(ContainSubstring("policy=Never expected=" + driverDigest))
	})
	It("uses only the matching pushed registry digest", func() {
		result, calls := runTarget("registry", "registry.example/target:test", "", []string{"other.example/target@sha256:" + strings.Repeat("b", 64), "registry.example/target@" + driverDigest})
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(calls).To(ContainElement([]string{"docker", "push", "registry.example/target:test"}))
		Expect(result.Stdout).To(ContainSubstring("selected=registry.example/target@" + driverDigest + " policy=Always"))
		Expect(hasCall(calls, "kind")).To(BeFalse())
	})
	DescribeTable("rejects invalid image selection before building", func(delivery, image string) {
		result, calls := runTarget(delivery, image, "", nil)
		Expect(result.Code).NotTo(Equal(0))
		Expect(calls).To(BeEmpty())
	}, Entry("missing registry image", "registry", ""), Entry("latest", "kind", "test:latest"), Entry("canary", "kind", "test:canary"), Entry("untagged", "kind", "test"), Entry("digest as local tag", "kind", "test@"+driverDigest))
	DescribeTable("propagates delivery failure", func(failure, delivery, image string) {
		result, _ := runTarget(delivery, image, failure, nil)
		Expect(result.Code).To(Equal(19), result.Stderr)
	}, Entry("container build", "docker build", "kind", "target:test"), Entry("kind load", "kind load", "kind", "target:test"), Entry("registry push", "docker push", "registry", "registry.example/target:test"))
	It("rejects an unknown repository digest", func() {
		result, _ := runTarget("registry", "registry.example/target:test", "", []string{"other.example/target@" + driverDigest})
		Expect(result.Code).NotTo(Equal(0))
		Expect(result.Stderr).To(ContainSubstring("unique pushed image digest"))
	})
})
