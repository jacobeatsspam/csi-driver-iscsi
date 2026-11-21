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

package iscsi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTargetdConfigValidation(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := TargetdConfig{BackendID: "nas", Endpoint: "https://localhost:18700", Pools: []string{"data"}, Namespace: "cluster", TargetIQN: "iqn.2026-01.test.example:target", Portals: []string{"192.0.2.1:3260"}, Username: "targetd", PasswordFile: password}
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write := func(content []byte) {
		t.Helper()
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(data)
	if _, err := NewTargetdControllerDriver("unix:///tmp/csi.sock", path); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	write(append(append([]byte(nil), data...), []byte(" {}")...))
	if _, err := NewTargetdControllerDriver("unix:///tmp/csi.sock", path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	for _, portals := range [][]string{{"192.0.2.1", "192.0.2.1:3260"}, {"nas.example.org:3260"}} {
		cfg.Portals = portals
		data, _ = json.Marshal(cfg)
		write(data)
		if _, err := NewTargetdControllerDriver("unix:///tmp/csi.sock", path); err == nil {
			t.Fatalf("accepted unsupported or duplicate portals %v", portals)
		}
	}
}
