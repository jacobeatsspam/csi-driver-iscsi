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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTeardownRefusesAnyCSIMount(t *testing.T) {
	for _, volume := range []string{"pvc-1234", "iscsi-pv-test"} {
		t.Run(volume, func(t *testing.T) {
			dir := t.TempDir()
			mountinfo := filepath.Join(dir, "mountinfo")
			line := "1 2 8:1 / /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/" + volume + "/mount rw - ext4 /dev/sda rw\n"
			if err := os.WriteFile(mountinfo, []byte(line), 0644); err != nil {
				t.Fatal(err)
			}
			docker := `#!/bin/sh
case "$1" in
  inspect) case "$2" in *control-plane) exit 0;; *) exit 1;; esac;;
  exec) case "$3" in ls) exit 0;; awk) /usr/bin/awk "$4" "$MOUNTINFO";; esac;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "deleted")
			if err := os.WriteFile(filepath.Join(dir, "kind"), []byte("#!/bin/sh\ntouch \"$MARKER\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "../../utils/teardown-kind.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "MOUNTINFO="+mountinfo, "MARKER="+marker)
			if err := cmd.Run(); err == nil {
				t.Fatal("teardown accepted a mounted CSI volume")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("kind cluster was deleted: %v", err)
			}
		})
	}
}
