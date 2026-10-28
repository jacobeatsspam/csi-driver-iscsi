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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Repository boilerplate validation", func() {
	checker := func(cwd string, args ...string) commandResult {
		cmd := exec.Command("python3", append([]string{filepath.Join(repoRoot, "hack/boilerplate/boilerplate.py")}, args...)...)
		cmd.Dir = cwd
		var out, err bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &err
		runErr := cmd.Run()
		if runErr != nil {
			Expect(runErr).To(BeAssignableToTypeOf(&exec.ExitError{}))
		}
		return commandResult{Code: cmd.ProcessState.ExitCode(), Stdout: out.String(), Stderr: err.String()}
	}
	It("detects missing headers using default references outside repository cwd", func() {
		tmp := GinkgoT().TempDir()
		bad := filepath.Join(tmp, "bad.go")
		writeFile(bad, "package bad\n", 0644)
		result := checker(tmp, bad)
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(strings.TrimSpace(result.Stdout)).To(Equal(bad))
	})
	It("accepts a correct header using default references", func() {
		tmp := GinkgoT().TempDir()
		good := filepath.Join(tmp, "good.go")
		header, err := os.ReadFile(filepath.Join(repoRoot, "hack/boilerplate/boilerplate.go.txt"))
		Expect(err).NotTo(HaveOccurred())
		writeFile(good, strings.ReplaceAll(string(header), "YEAR", "2026")+"\npackage good\n", 0644)
		result := checker(tmp, good)
		Expect(result.Code).To(Equal(0), result.Stderr)
		Expect(result.Stdout).To(BeEmpty())
	})
	It("fails instead of passing when the reference directory is empty", func() {
		tmp := GinkgoT().TempDir()
		result := checker(repoRoot, "--boilerplate-dir="+tmp)
		Expect(result.Code).NotTo(Equal(0))
		Expect(result.Stderr).To(ContainSubstring("No boilerplate references"))
	})
})
