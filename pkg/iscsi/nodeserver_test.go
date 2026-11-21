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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeID(t *testing.T) {
	id := EncodeNodeID("worker-1", "iqn.1993-08.org.debian:node-1")
	node, initiator, err := DecodeNodeID(id)
	if err != nil || node != "worker-1" || initiator != "iqn.1993-08.org.debian:node-1" {
		t.Fatalf("DecodeNodeID = %q, %q, %v", node, initiator, err)
	}
	for _, id := range []string{"worker-1", "v2:YQ:Yg", "v1::Yg", "v1:YQ:YmFk", EncodeNodeID(strings.Repeat("a", 256), "iqn.1993-08.org.debian:node")} {
		if _, _, err := DecodeNodeID(id); err == nil {
			t.Errorf("accepted malformed node ID %q", id)
		}
	}
}

func TestReadInitiator(t *testing.T) {
	file := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{"# comment\nInitiatorName=iqn.1993-08.org.debian:one\n", true},
		{"", false},
		{"InitiatorName=bad\n", false},
		{"InitiatorName=iqn.one\nInitiatorName=iqn.two\n", false},
	} {
		if err := os.WriteFile(file, []byte(tc.data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readInitiator(file)
		if (err == nil) != tc.valid {
			t.Errorf("readInitiator(%q): %v", tc.data, err)
		}
	}
}

func TestInterfaceInitiator(t *testing.T) {
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{"iface.initiatorname = <empty>", true},
		{"iface.initiatorname = iqn.test", true},
		{"iface.initiatorname = iqn.other", false},
		{"", false},
		{"iface.initiatorname = iqn.test\niface.initiatorname = iqn.test", false},
	} {
		if err := checkInterfaceInitiator(tc.text, "iqn.test"); (err == nil) != tc.valid {
			t.Errorf("checkInterfaceInitiator(%q): %v", tc.text, err)
		}
	}
}

func TestNodeRequestsRejectUnsupportedInput(t *testing.T) {
	ns := &nodeServer{Driver: &driver{nodeID: "worker"}}
	info, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil || info.NodeId != "worker" {
		t.Fatalf("static node ID: %v, %v", info, err)
	}
	_, err = ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "vol", TargetPath: "/tmp/unused", VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("block volume: %v", err)
	}
	req := &csi.NodePublishVolumeRequest{VolumeId: "vol", TargetPath: "/tmp/unused", VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}, PublishContext: map[string]string{"volumeUUID": "aabb"}}
	_, err = ns.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("dynamic volume on static node: %v", err)
	}
	ns.Driver.dynamicNode = true
	_, err = ns.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing dynamic initiator: %v", err)
	}
	req.VolumeId = "td1_backend_pool_csitd_namespace_name_policy_uuid"
	req.PublishContext = nil
	_, err = ns.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("dynamic handle without context: %v", err)
	}
	req.PublishContext = map[string]string{"initiatorName": "iqn.1993-08.org.debian:node", "volumeUUID": "wrong"}
	_, err = ns.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("dynamic handle with different UUID: %v", err)
	}
	_, err = ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "../escape", TargetPath: "/tmp/unused"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsafe volume ID: %v", err)
	}
}

func TestDynamicNodeRequiresControllerConnection(t *testing.T) {
	ns := &nodeServer{Driver: &driver{dynamicNode: true}}
	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "td1_backend_pool_csitd_namespace_request_" + policyHash("ext4") + "_uuid",
		TargetPath: "/unused",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{"lun": "9"},
		PublishContext: map[string]string{
			"iqn": "iqn.1993-08.org.debian:target", "targetPortal": "127.0.0.1:3260",
			"portals": "[]", "initiatorName": "iqn.1993-08.org.debian:node", "volumeUUID": "uuid",
		},
	}
	if _, err := ns.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "missing lun") {
		t.Fatalf("used LUN from volume attributes: %v", err)
	}
	req.PublishContext["lun"] = "9"
	req.VolumeContext["sessionCHAPAuth"] = "true"
	if _, err := ns.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "CHAP") {
		t.Fatalf("accepted CHAP override: %v", err)
	}
	delete(req.VolumeContext, "sessionCHAPAuth")
	req.VolumeCapability.GetMount().FsType = "xfs"
	if _, err := ns.NodePublishVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("accepted a filesystem incompatible with the volume: %v", err)
	}
}
