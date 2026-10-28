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
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Re-executing this test binary provides command doubles without another
// interpreter or a separate build for every external command.
func TestMain(m *testing.M) {
	if mode := os.Getenv("E2E_GO_FAKE"); mode != "" {
		fake, ok := commandFakes[mode]
		if !ok {
			os.Exit(127)
		}
		os.Exit(fake(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestE2EHarness(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "iSCSI E2E Harness Suite")
}
