/*
Copyright 2017 The Kubernetes Authors.

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
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"

	iscsiLib "github.com/kubernetes-csi/csi-driver-iscsi/pkg/iscsilib"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nodeServer struct {
	Driver *driver
	csi.UnimplementedNodeServer
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volumeID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "targetPath not provided")
	}
	if req.GetVolumeCapability().GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "only filesystem volumes are supported")
	}
	dynamic := strings.HasPrefix(req.GetVolumeId(), "td1_") || req.GetPublishContext()["initiatorName"] != "" || req.GetPublishContext()["volumeUUID"] != ""
	if dynamic && !ns.Driver.dynamicNode {
		return nil, status.Error(codes.FailedPrecondition, "dynamic volumes require dynamic node mode")
	}
	if dynamic {
		for _, key := range []string{"iqn", "targetPortal", "portals", "lun", "initiatorName", "volumeUUID"} {
			if req.GetPublishContext()[key] == "" {
				return nil, status.Errorf(codes.InvalidArgument, "dynamic publish context is missing %s", key)
			}
		}
		parts := strings.Split(req.GetVolumeId(), "_")
		if len(parts) != 8 || parts[0] != "td1" || parts[3] != "csitd" || parts[7] != req.GetPublishContext()["volumeUUID"] || !safeTargetdID.MatchString(parts[7]) {
			return nil, status.Error(codes.InvalidArgument, "dynamic volume ID and UUID do not match")
		}
		if err := validateCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
			return nil, err
		}
		if parts[6] != policyHash(filesystem(req.GetVolumeCapability())) {
			return nil, status.Error(codes.FailedPrecondition, "volume filesystem does not match capability")
		}
		for _, attributes := range []map[string]string{req.GetVolumeContext(), req.GetPublishContext()} {
			if iface := attributes["iscsiInterface"]; iface != "" && iface != "default" {
				return nil, status.Error(codes.InvalidArgument, "dynamic volumes require the default iSCSI interface")
			}
			if attributes["secret"] != "" || attributes["sessionCHAPAuth"] == "true" || attributes["discoveryCHAPAuth"] == "true" {
				return nil, status.Error(codes.InvalidArgument, "dynamic volumes do not support CHAP")
			}
		}
		initiator, err := hostInitiator()
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		if req.GetPublishContext()["initiatorName"] != initiator {
			return nil, status.Error(codes.FailedPrecondition, "host initiator differs from the published initiator")
		}
	}

	iscsiInfo, err := getISCSIInfo(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	diskMounter := getISCSIDiskMounter(iscsiInfo, req)

	util := &ISCSIUtil{}
	if _, err := util.AttachDisk(*diskMounter); err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if !validVolumeID(req.GetVolumeId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid volume ID")
	}
	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	diskUnmounter := getISCSIDiskUnmounter(req)

	iscsiutil := &ISCSIUtil{}
	if err := iscsiutil.DetachDisk(*diskUnmounter, targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	id := ns.Driver.nodeID
	if ns.Driver.dynamicNode {
		initiator, err := hostInitiator()
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		id = EncodeNodeID(id, initiator)
		if len(id) > 256 || ns.Driver.nodeID == "" {
			return nil, status.Error(codes.FailedPrecondition, "dynamic node ID is empty or exceeds 256 bytes")
		}
	}
	return &csi.NodeGetInfoResponse{NodeId: id}, nil
}

func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, in *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// EncodeNodeID carries the host initiator to the controller without a node lookup.
func EncodeNodeID(node, initiator string) string {
	return "v1:" + base64.RawURLEncoding.EncodeToString([]byte(node)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(initiator))
}

func DecodeNodeID(id string) (string, string, error) {
	parts := strings.Split(id, ":")
	if len(id) > 256 || len(parts) != 3 || parts[0] != "v1" {
		return "", "", fmt.Errorf("invalid dynamic node ID")
	}
	node, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(node) == 0 {
		return "", "", fmt.Errorf("invalid dynamic node ID")
	}
	initiator, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !validInitiator(string(initiator)) {
		return "", "", fmt.Errorf("invalid dynamic node ID")
	}
	return string(node), string(initiator), nil
}

var initiatorPattern = regexp.MustCompile(`^iqn\.[0-9]{4}-(0[1-9]|1[0-2])\.[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[a-z0-9._:-]+)?$`)

func validInitiator(name string) bool {
	return len(name) <= 223 && initiatorPattern.MatchString(name)
}

func readInitiator(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read host initiator: %w", err)
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "InitiatorName" {
			continue
		}
		if name != "" {
			return "", fmt.Errorf("multiple host initiator names")
		}
		name = strings.TrimSpace(value)
		if !validInitiator(name) {
			return "", fmt.Errorf("invalid host initiator name")
		}
	}
	if name == "" {
		return "", fmt.Errorf("host initiator name missing")
	}
	return name, nil
}

func hostInitiator() (string, error) {
	name, err := readInitiator("/host/etc/iscsi/initiatorname.iscsi")
	if err != nil {
		return "", err
	}
	iface, err := iscsiLib.ShowInterface("default")
	if err != nil {
		return "", fmt.Errorf("read default iSCSI interface: %w", err)
	}
	if err := checkInterfaceInitiator(iface, name); err != nil {
		return "", err
	}
	return name, nil
}

func checkInterfaceInitiator(iface, name string) error {
	found := false
	for _, line := range strings.Split(iface, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "iface.initiatorname" {
			continue
		}
		if found {
			return fmt.Errorf("multiple default interface initiator names")
		}
		found = true
		value = strings.TrimSpace(value)
		if value != "" && value != "<empty>" && value != name {
			return fmt.Errorf("default interface initiator differs from host initiator")
		}
	}
	if !found {
		return fmt.Errorf("default interface initiator is unavailable")
	}
	return nil
}
