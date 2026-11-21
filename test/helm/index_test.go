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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartIndexAndArchive(t *testing.T) {
	const chart = "csi-driver-iscsi"
	const archive = "latest/csi-driver-iscsi-v0.0.0.tgz"
	indexFile, err := os.Open("../../charts/index.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer indexFile.Close()
	var index struct {
		Entries map[string][]struct {
			Digest  string   `json:"digest"`
			URLs    []string `json:"urls"`
			Version string   `json:"version"`
		} `json:"entries"`
	}
	if err := yaml.NewYAMLOrJSONDecoder(indexFile, 4096).Decode(&index); err != nil {
		t.Fatal(err)
	}
	entries := index.Entries[chart]
	var digest string
	for _, entry := range entries {
		if entry.Version == "v0.0.0" && len(entry.URLs) == 1 && entry.URLs[0] == archive {
			if digest != "" {
				t.Fatal("duplicate latest chart entry")
			}
			digest = entry.Digest
		}
	}
	if digest == "" {
		t.Fatalf("latest chart missing from index: %+v", entries)
	}
	data, err := os.ReadFile(filepath.Join("../../charts", archive))
	if err != nil {
		t.Fatal(err)
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(data)); digest != actual {
		t.Fatal("chart archive digest does not match index")
	}
	files := map[string][]byte{}
	root := filepath.Join("../../charts/latest", chart)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(name)], err = os.ReadFile(path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name, ok := strings.CutPrefix(header.Name, chart+"/")
		if !ok || header.Typeflag != tar.TypeReg {
			t.Fatalf("unexpected archive entry %q", header.Name)
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		expected, ok := files[name]
		if ok && name == "Chart.yaml" {
			contents, err = yaml.ToJSON(contents)
			if err != nil {
				t.Fatal(err)
			}
			expected, err = yaml.ToJSON(expected)
			if err != nil {
				t.Fatal(err)
			}
		}
		if !ok || !bytes.Equal(contents, expected) {
			t.Fatalf("archive entry %q does not match chart source", name)
		}
		delete(files, name)
	}
	if len(files) != 0 {
		t.Fatalf("chart files missing from archive: %v", files)
	}
}
