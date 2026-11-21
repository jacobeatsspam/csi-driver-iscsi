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
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestGetISCSIInfoPortals(t *testing.T) {
	tests := []struct {
		name     string
		portals  *string
		expected []string
	}{
		{
			name:     "targetPortal only",
			expected: []string{"10.0.0.1:3260"},
		},
		{
			name:     "empty portals",
			portals:  strPtr("[]"),
			expected: []string{"10.0.0.1:3260"},
		},
		{
			name:     "targetPortal first, then portals in order",
			portals:  strPtr(`["10.0.0.3:3261", "10.0.0.2"]`),
			expected: []string{"10.0.0.1:3260", "10.0.0.3:3261", "10.0.0.2:3260"},
		},
		{
			name:     "portals repeating targetPortal and each other",
			portals:  strPtr(`["10.0.0.1", "10.0.0.2:3260", "10.0.0.1:3260", "10.0.0.2"]`),
			expected: []string{"10.0.0.1:3260", "10.0.0.2:3260"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			volumeContext := map[string]string{
				"targetPortal": "10.0.0.1",
				"iqn":          "iqn.2001-04.com.example:storage.disk1",
				"lun":          "0",
			}
			if test.portals != nil {
				volumeContext["portals"] = *test.portals
			}
			req := &csi.NodePublishVolumeRequest{VolumeId: "vol", VolumeContext: volumeContext}

			info, err := getISCSIInfo(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(info.Portals, test.expected) {
				t.Errorf("Portals = %v, expected %v", info.Portals, test.expected)
			}
			if c := buildISCSIConnector(info); !reflect.DeepEqual(c.TargetPortals, test.expected) {
				t.Errorf("connector TargetPortals = %v, expected %v", c.TargetPortals, test.expected)
			}
		})
	}
}

func TestGetISCSIInfoInvalidPortals(t *testing.T) {
	req := &csi.NodePublishVolumeRequest{
		VolumeId: "vol",
		VolumeContext: map[string]string{
			"targetPortal": "10.0.0.1",
			"iqn":          "iqn.2001-04.com.example:storage.disk1",
			"lun":          "0",
			"portals":      "10.0.0.2",
		},
	}
	if _, err := getISCSIInfo(req); err == nil {
		t.Error("expected an error for portals that are not a JSON list")
	}
}

func strPtr(s string) *string { return &s }

func TestGetISCSIInfoPublishContext(t *testing.T) {
	volume := map[string]string{"targetPortal": "10.0.0.1", "iqn": "iqn.test"}
	for _, tc := range []struct {
		name    string
		publish map[string]string
		valid   bool
	}{
		{"dynamic", map[string]string{"targetPortal": "10.0.0.1", "iqn": "iqn.test", "lun": "4"}, true},
		{"equivalent default port", map[string]string{"targetPortal": "10.0.0.1:3260", "lun": "4"}, true},
		{"conflicting target", map[string]string{"targetPortal": "10.0.0.2", "lun": "4"}, false},
		{"negative lun", map[string]string{"lun": "-1"}, false},
		{"overflow lun", map[string]string{"lun": "16384"}, false},
		{"empty portal", map[string]string{"targetPortal": ":3260", "lun": "4"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := getISCSIInfo(&csi.NodePublishVolumeRequest{VolumeId: "vol", VolumeContext: volume, PublishContext: tc.publish})
			if (err == nil) != tc.valid {
				t.Fatalf("getISCSIInfo: %v", err)
			}
		})
	}
	info, err := getISCSIInfo(&csi.NodePublishVolumeRequest{VolumeId: "vol", VolumeContext: volume, PublishContext: map[string]string{"lun": "4", "initiatorName": "iqn.1993-08.org.debian:node", "volumeUUID": "aabb-ccdd"}})
	if err != nil || !info.discovery {
		t.Fatalf("dynamic volume must discover target: %v", err)
	}
	if _, err := getISCSIInfo(&csi.NodePublishVolumeRequest{VolumeId: "../vol", VolumeContext: map[string]string{"targetPortal": "host", "iqn": "iqn.test", "lun": "0"}}); err == nil {
		t.Fatal("accepted unsafe volume ID")
	}
}

func TestLegacySecretsSuppressRPCPayloadLogs(t *testing.T) {
	legacy := map[string]string{"secret": "legacy CHAP credentials"}
	for _, req := range []interface{}{
		&csi.NodePublishVolumeRequest{VolumeContext: legacy},
		&csi.NodePublishVolumeRequest{PublishContext: legacy},
		&csi.ValidateVolumeCapabilitiesRequest{VolumeContext: legacy},
		&csi.CreateVolumeRequest{Parameters: legacy},
	} {
		if !hasLegacySecret(req) {
			t.Fatalf("legacy credentials would appear in the %T log", req)
		}
	}
	if hasLegacySecret(&csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"lun": "0"}}) {
		t.Fatal("suppressed ordinary request diagnostics")
	}
}
