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

package targetd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequiresExplicitDisposableVM(t *testing.T) {
	for _, tc := range []struct{ name, opt, virt string }{
		{"no opt in", "", "0"},
		{"not a VM", "1", "1"},
		{"wrong cluster", "1", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			for name, body := range map[string]string{
				"systemd-detect-virt": "#!/bin/sh\nexit " + tc.virt + "\n",
				"kubectl":             "#!/bin/sh\necho unrelated-context\n",
				"apt-get":             "#!/bin/sh\ntouch \"$MARKER\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "run.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_DISPOSABLE_VM="+tc.opt, "MARKER="+marker)
			if err := cmd.Run(); err == nil {
				t.Fatal("fixture ran without a disposable VM")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("fixture touched the host: %v", err)
			}
		})
	}
}

func TestCSIRequiresExplicitDisposableVM(t *testing.T) {
	cmd := exec.Command("bash", "csi-test.sh")
	cmd.Env = append(os.Environ(), "E2E_DISPOSABLE_VM=")
	if err := cmd.Run(); err == nil {
		t.Fatal("CSI fixture ran without a disposable VM")
	}
}

func TestInitiatorRestoreFailurePreservesFixtureAndBackup(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Index(string(source), "restore_initiator() {")
	b := strings.Index(string(source), "cleanup() {")
	if a < 0 || b <= a {
		t.Fatal("restore function missing")
	}
	function := strings.ReplaceAll(string(source[a:b]), "/etc/iscsi", dir)
	original := filepath.Join(dir, "initiatorname.iscsi")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "initiatorname.iscsi"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	command := function + "\nwork=\"" + work + "\"; initiator_changed=1; initiator_existed=1; if restore_initiator; then exit 3; fi; test \"$initiator_changed\" = 1"
	cmd := exec.Command("bash", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore failure was mishandled: %v %s", err, out)
	}
	got, err := os.ReadFile(original)
	if err != nil || string(got) != "fixture" {
		t.Fatalf("fixture overwritten: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(work, "initiatorname.iscsi"))
	if err != nil || string(got) != "original" {
		t.Fatalf("backup lost: %q %v", got, err)
	}
}

func TestKindDaemonRejectsStaleNodePID(t *testing.T) {
	dir := t.TempDir()
	docker := `#!/bin/sh
case "$*" in
  *Labels*) echo kind;;
  *State.Pid*) echo 123;;
  *Config.Env*) echo NODE_PID=456;;
  *start*) touch "$MARKER";;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "started")
	cmd := exec.Command("bash", "kind-daemon.sh", "start")
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "MARKER="+marker, "E2E_KIND_CLUSTER=kind")
	if err := cmd.Run(); err == nil {
		t.Fatal("stale daemon accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale daemon started: %v", err)
	}
}

func TestWaitUnpublishedRequiresMountAndConnectorRemoval(t *testing.T) {
	source, err := os.ReadFile("csi-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Index(string(source), "wait_unpublished() {")
	if a < 0 {
		t.Fatal("unpublish gate missing")
	}
	b := strings.Index(string(source[a:]), "\n}\n")
	if b < 0 {
		t.Fatal("unpublish gate end missing")
	}
	function := strings.Replace(string(source[a:a+b+2]), "i<120", "i<2", 1)
	for _, tc := range []struct {
		name, mount, record, directory string
		ok                             bool
	}{
		{"all removed", "", "", "1", true},
		{"mount remains", "1", "", "1", false},
		{"record remains", "", "1", "1", false},
		{"directory unavailable", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			docker := `#!/bin/sh
case "$3" in
  cat) [ -z "$MOCK_MOUNT" ] || echo /volumes/kubernetes.io~csi/pv-test/mount; exit 0;;
  sh) [ "$MOCK_DIRECTORY" = 1 ] && [ -z "$MOCK_RECORD" ];;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", "node=kind-control-plane\n"+function+"\nwait_unpublished pv-test vol-test")
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "MOCK_MOUNT="+tc.mount, "MOCK_RECORD="+tc.record, "MOCK_DIRECTORY="+tc.directory)
			if err := cmd.Run(); (err == nil) != tc.ok {
				t.Fatalf("unexpected gate result: %v", err)
			}
		})
	}
}
