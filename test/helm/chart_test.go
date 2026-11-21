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

package helm_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func render(t *testing.T, extra ...string) map[string]map[string]interface{} {
	t.Helper()
	args := []string{
		"--set", "targetd.enabled=true",
		"--set-string", "image.iscsi.repository=example.test/driver",
		"--set-string", "image.iscsi.tag=revision",
		"--set-string", "targetd.backendID=backend",
		"--set-string", "targetd.endpoint=https://storage.example:18700/targetrpc",
		"--set-string", "targetd.pools[0]=vg-test",
		"--set-string", "targetd.namespace=cluster",
		"--set-string", "targetd.targetIQN=iqn.2026-01.org.example:storage",
		"--set-string", "targetd.portals[0]=192.0.2.20:3260",
		"--set-string", "targetd.existingSecret=test-credentials",
	}
	return renderWithArgs(t, append(args, extra...)...)
}

func renderWithArgs(t *testing.T, extra ...string) map[string]map[string]interface{} {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := []string{"template", "test", "../../charts/latest/csi-driver-iscsi", "--namespace", "storage-test"}
	output, err := exec.Command("helm", append(args, extra...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	objects := map[string]map[string]interface{}{}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	for {
		var object map[string]interface{}
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object == nil {
			continue
		}
		kind, _, _ := unstructured.NestedString(object, "kind")
		name, _, _ := unstructured.NestedString(object, "metadata", "name")
		key := kind + "/" + name
		if _, exists := objects[key]; exists {
			t.Fatalf("duplicate resource %s", key)
		}
		objects[key] = object
	}
	return objects
}

func TestTargetdChart(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		ca   string
	}{
		{name: "system CA"},
		{name: "private CA", args: []string{"--set-string", "targetd.existingCAConfigMap=private-ca", "--set-string", "node.stateDir=/var/run/iscsi.csi.k8s.io", "--set-string", "imagePullSecrets[0].name=registry"}, ca: "/etc/targetd/ca/ca.crt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := render(t, tc.args...)
			config := objects["ConfigMap/csi-targetd-config"]
			value, found, _ := unstructured.NestedString(config, "data", "config.json")
			if !found {
				t.Fatal("missing targetd configuration")
			}
			var settings map[string]interface{}
			if err := json.Unmarshal([]byte(value), &settings); err != nil {
				t.Fatal(err)
			}
			if settings["backendID"] != "backend" || settings["namespace"] != "cluster" || settings["passwordFile"] != "/etc/targetd/credentials/password" {
				t.Fatalf("unexpected controller configuration: %v", settings)
			}
			ca, _ := settings["caFile"].(string)
			if ca != tc.ca {
				t.Fatalf("CA path = %q, want %q", ca, tc.ca)
			}
			barrier := objects["ConfigMap/csi-iscsi-targetd-barrier"]
			keep, _, _ := unstructured.NestedString(barrier, "metadata", "annotations", "helm.sh/resource-policy")
			if keep != "keep" {
				t.Fatal("operation barrier is not retained")
			}
			controller := objects["Deployment/csi-iscsi-controller"]
			attach, found, _ := unstructured.NestedBool(objects["CSIDriver/iscsi.csi.k8s.io"], "spec", "attachRequired")
			if !found || !attach {
				t.Fatal("targetd driver must require controller attachment")
			}
			strategy, _, _ := unstructured.NestedString(controller, "spec", "strategy", "type")
			replicas, _, _ := unstructured.NestedFieldNoCopy(controller, "spec", "replicas")
			if strategy != "Recreate" || replicas != float64(1) {
				t.Fatalf("unsafe controller rollout: replicas=%v strategy=%s", replicas, strategy)
			}
			for _, key := range []string{"Deployment/csi-iscsi-controller", "DaemonSet/csi-iscsi-node"} {
				spec, _, _ := unstructured.NestedMap(objects[key], "spec", "template", "spec")
				containers, _, _ := unstructured.NestedSlice(spec, "containers")
				foundImage := false
				for _, item := range containers {
					container := item.(map[string]interface{})
					if container["name"] == "iscsi" && container["image"] == "example.test/driver:revision" {
						foundImage = true
					}
				}
				if !foundImage {
					t.Fatalf("%s does not use the selected image", key)
				}
				if tc.ca != "" {
					pullSecrets, _, _ := unstructured.NestedSlice(spec, "imagePullSecrets")
					if len(pullSecrets) != 1 || pullSecrets[0].(map[string]interface{})["name"] != "registry" {
						t.Fatalf("%s does not use the selected image pull secret", key)
					}
				}
				volumes, _, _ := unstructured.NestedSlice(spec, "volumes")
				foundExpectedVolume := false
				for _, item := range volumes {
					volume := item.(map[string]interface{})
					if key == "Deployment/csi-iscsi-controller" && volume["name"] == "ca" {
						name, _, _ := unstructured.NestedString(volume, "configMap", "name")
						optional, _, _ := unstructured.NestedBool(volume, "configMap", "optional")
						if tc.ca == "" && (name != "csi-targetd-ca" || !optional) || tc.ca != "" && (name != "private-ca" || optional) {
							t.Fatalf("unexpected CA volume: %v", volume)
						}
					}
					if key == "Deployment/csi-iscsi-controller" && volume["name"] == "credentials" {
						name, _, _ := unstructured.NestedString(volume, "secret", "secretName")
						if name != "test-credentials" {
							t.Fatalf("credential Secret = %q", name)
						}
						foundExpectedVolume = true
					}
					if key == "DaemonSet/csi-iscsi-node" && volume["name"] == "iscsi-csi-run-dir" {
						path, _, _ := unstructured.NestedString(volume, "hostPath", "path")
						dirType, _, _ := unstructured.NestedString(volume, "hostPath", "type")
						want := "/var/lib/iscsi.csi.k8s.io"
						if tc.ca != "" {
							want = "/var/run/iscsi.csi.k8s.io"
						}
						if path != want || dirType != "DirectoryOrCreate" {
							t.Fatalf("unexpected node state directory: %v", volume["hostPath"])
						}
						foundExpectedVolume = true
					}
				}
				if !foundExpectedVolume {
					t.Fatalf("%s is missing its expected volume", key)
				}
			}
		})
	}
}

func TestTargetdChartRequiresConfiguration(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	output, err := exec.Command("helm", "template", "test", "../../charts/latest/csi-driver-iscsi", "--set", "targetd.enabled=true").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "is required") {
		t.Fatalf("expected required configuration error: %v\n%s", err, output)
	}
}

func TestStaticChart(t *testing.T) {
	objects := renderWithArgs(t)
	driver := objects["CSIDriver/iscsi.csi.k8s.io"]
	attach, found, _ := unstructured.NestedBool(driver, "spec", "attachRequired")
	if !found || attach {
		t.Fatal("static driver must not require controller attachment")
	}
	if objects["Deployment/csi-iscsi-controller"] != nil || objects["ConfigMap/csi-targetd-config"] != nil {
		t.Fatal("static installation must not create targetd resources")
	}
	node := objects["DaemonSet/csi-iscsi-node"]
	token, found, _ := unstructured.NestedBool(node, "spec", "template", "spec", "automountServiceAccountToken")
	if !found || token {
		t.Fatal("node driver must not mount a Kubernetes API token")
	}
	tolerations, _, _ := unstructured.NestedSlice(node, "spec", "template", "spec", "tolerations")
	if len(tolerations) == 0 || tolerations[0].(map[string]interface{})["operator"] != "Exists" {
		t.Fatal("node driver must tolerate node taints")
	}
}

func TestDigestPinnedDriverImage(t *testing.T) {
	image := "example.test/driver@sha256:" + strings.Repeat("a", 64)
	objects := render(t, "--set-string", "image.iscsi.repository="+image)
	for _, key := range []string{"Deployment/csi-iscsi-controller", "DaemonSet/csi-iscsi-node"} {
		containers, _, _ := unstructured.NestedSlice(objects[key], "spec", "template", "spec", "containers")
		found := false
		for _, item := range containers {
			container := item.(map[string]interface{})
			if container["name"] == "iscsi" && container["image"] == image {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not use the digest-pinned image", key)
		}
	}
}

func TestTargetdConfigChangeRestartsController(t *testing.T) {
	first := render(t)["Deployment/csi-iscsi-controller"]
	second := render(t, "--set-string", "targetd.backendID=other-backend")["Deployment/csi-iscsi-controller"]
	one, _, _ := unstructured.NestedString(first, "spec", "template", "metadata", "annotations", "checksum/config")
	two, _, _ := unstructured.NestedString(second, "spec", "template", "metadata", "annotations", "checksum/config")
	if one == "" || two == "" || one == two {
		t.Fatal("controller pod checksum does not change with targetd configuration")
	}
}

func TestChartOverrides(t *testing.T) {
	objects := render(t,
		"--set-string", "kubeletDir=/srv/kubelet",
		"--set-string", "image.csiAttacher.tag=custom",
		"--set", "storageClass.create=true",
		"--set-string", "storageClass.pool=vg-test",
	)
	class := objects["StorageClass/iscsi-targetd"]
	pool, found, _ := unstructured.NestedString(class, "parameters", "pool")
	if !found || pool != "vg-test" {
		t.Fatal("StorageClass does not use the selected pool")
	}
	node := objects["DaemonSet/csi-iscsi-node"]
	volumes, _, _ := unstructured.NestedSlice(node, "spec", "template", "spec", "volumes")
	foundKubelet := false
	for _, item := range volumes {
		volume := item.(map[string]interface{})
		if volume["name"] == "socket-dir" {
			path, _, _ := unstructured.NestedString(volume, "hostPath", "path")
			foundKubelet = path == "/srv/kubelet/plugins/iscsi.csi.k8s.io"
		}
	}
	if !foundKubelet {
		t.Fatal("node does not use the selected kubelet directory")
	}
	containers, _, _ := unstructured.NestedSlice(objects["Deployment/csi-iscsi-controller"], "spec", "template", "spec", "containers")
	foundImage := false
	for _, item := range containers {
		container := item.(map[string]interface{})
		if container["name"] == "attacher" && container["image"] == "registry.k8s.io/sig-storage/csi-attacher:custom" {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatal("controller does not use the selected attacher image")
	}
}
