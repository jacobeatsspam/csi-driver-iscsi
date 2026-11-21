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

package isolation

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, env ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	stubs := map[string]string{
		"id":   "#!/bin/sh\necho 0\n",
		"ls":   "#!/bin/sh\nif [ \"$LS_FAIL\" = 1 ]; then exit 1; fi\n/bin/ls \"$@\"\n",
		"stat": "#!/bin/sh\necho 0\n",
		"systemctl": `#!/bin/sh
echo "$*" >> "$CALLS"
case "$1:$2" in
  show:iscsid.service) echo "${SERVICE_ACTIVE:-inactive}";;
  show:iscsid.socket) echo "${SOCKET_ACTIVE:-active}";;
  is-enabled:iscsid.service) echo "${SERVICE_ENABLED:-enabled}";;
  is-enabled:iscsid.socket) echo "${SOCKET_ENABLED:-enabled}";;
esac
`,
	}
	for name, data := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../with-iscsid-isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	if err = os.Mkdir(sessions, 0755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(source), "/sys/class/iscsi_session", sessions)
	script = strings.ReplaceAll(script, "/run/lock/csi-iscsi-e2e", filepath.Join(dir, "lock"))
	scriptPath := filepath.Join(dir, "isolation.sh")
	if err = os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", scriptPath, "/bin/sh", "-c", "if [ -n \"$CREATE_SESSION\" ]; then touch \"$SESSIONS/session1\"; fi; if [ -n \"$REMOVE_SESSIONS\" ]; then rmdir \"$SESSIONS\"; fi; exit ${TEST_EXIT:-0}")

	cmd.Env = append(os.Environ(), append([]string{"PATH=" + dir + ":/usr/bin:/bin", "CALLS=" + log, "SESSIONS=" + sessions}, env...)...)
	err = cmd.Run()
	calls, _ := os.ReadFile(log)
	return string(calls), err
}

func TestIsolationRestoresServiceMask(t *testing.T) {
	calls, err := run(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mask --runtime iscsid.service", "unmask --runtime iscsid.service"} {
		if !strings.Contains(calls, want+"\n") {
			t.Errorf("missing %q in %s", want, calls)
		}
	}
}

func TestIsolationPreservesMasksOnFailure(t *testing.T) {
	calls, err := run(t, "SERVICE_ENABLED=masked", "SOCKET_ENABLED=masked-runtime", "SOCKET_ACTIVE=inactive", "TEST_EXIT=7")
	if err == nil {
		t.Fatal("test failure was swallowed")
	}
	if strings.Contains(calls, "mask --runtime") || strings.Contains(calls, "unmask") {
		t.Fatalf("changed pre-existing masks: %s", calls)
	}
}

func TestIsolationRefusesActiveDaemon(t *testing.T) {
	calls, err := run(t, "SERVICE_ACTIVE=active")
	if err == nil || strings.Contains(calls, "mask ") || strings.Contains(calls, "stop ") {
		t.Fatalf("active daemon was not left alone: err=%v calls=%s", err, calls)
	}
}

func TestRegistryDeliveryLeavesHostAlone(t *testing.T) {
	calls, err := run(t, "E2E_DRIVER_DELIVERY=registry")
	if err != nil || calls != "" {
		t.Fatalf("registry delivery touched host: err=%v calls=%s", err, calls)
	}
}

func TestIsolationRetainsMaskAfterFailureOrSession(t *testing.T) {
	for _, env := range []string{"TEST_EXIT=7", "CREATE_SESSION=1", "REMOVE_SESSIONS=1"} {
		t.Run(env, func(t *testing.T) {
			calls, err := run(t, env)
			if err == nil || strings.Contains(calls, "unmask --runtime iscsid.service\n") {
				t.Fatalf("unsafe restoration: err=%v calls=%s", err, calls)
			}
		})
	}
}
func TestIsolationRejectsUnobservableSessions(t *testing.T) {
	calls, err := run(t, "LS_FAIL=1")
	if err == nil || strings.Contains(calls, "mask --runtime") {
		t.Fatalf("unobservable sessions were accepted: err=%v calls=%s", err, calls)
	}
}

func TestIsolationRejectsConcurrentRun(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"id":        "#!/bin/sh\necho 0\n",
		"stat":      "#!/bin/sh\necho 0\n",
		"systemctl": "#!/bin/sh\ncase \"$1\" in show) echo inactive;; is-enabled) echo enabled;; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	sessions := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessions, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../with-iscsid-isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(source), "/sys/class/iscsi_session", sessions)
	script = strings.ReplaceAll(script, "/run/lock/csi-iscsi-e2e", filepath.Join(dir, "lock"))
	scriptPath := filepath.Join(dir, "isolation.sh")
	if err = os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
	first := exec.Command("/bin/bash", scriptPath, "/bin/sh", "-c", "echo ready; read done")
	first.Env = env
	stdin, err := first.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); first.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("first test did not start: %q %v", line, err)
	}
	second := exec.Command("/bin/bash", scriptPath, "/bin/true")
	second.Env = env
	out, err := second.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "owns the host lock") {
		t.Fatalf("second test was not blocked: %v %s", err, out)
	}
	if _, err = stdin.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if err = first.Wait(); err != nil {
		t.Fatal(err)
	}
	nested := exec.Command("/bin/bash", scriptPath, "--lock-only", "/bin/bash", scriptPath, "/bin/true")
	nested.Env = env
	if out, err = nested.CombinedOutput(); err != nil {
		t.Fatalf("nested lock failed: %v %s", err, out)
	}
}
