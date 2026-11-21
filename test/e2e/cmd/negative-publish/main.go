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

// negative-publish attempts a real node publish with an intentionally wrong serial.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("expected PV and VolumeAttachment JSON paths")
	}
	var pv v1.PersistentVolume
	var attachment storagev1.VolumeAttachment
	for i, target := range []any{&pv, &attachment} {
		data, err := os.ReadFile(os.Args[i+1])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, target); err != nil {
			return err
		}
	}
	if pv.Spec.CSI == nil || !attachment.Status.Attached || attachment.Status.AttachmentMetadata["volumeUUID"] == "" {
		return fmt.Errorf("fixture volume is not attached")
	}
	parts := strings.Split(pv.Spec.CSI.VolumeHandle, "_")
	if len(parts) != 8 || parts[7] != attachment.Status.AttachmentMetadata["volumeUUID"] {
		return fmt.Errorf("unexpected fixture volume identity")
	}
	parts[7] = "wrong-serial"
	attachment.Status.AttachmentMetadata["volumeUUID"] = parts[7]
	conn, err := grpc.NewClient("unix:///var/lib/kubelet/plugins/iscsi.csi.k8s.io/csi.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err = csi.NewNodeClient(conn).NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId: strings.Join(parts, "_"), TargetPath: "/var/lib/kubelet/plugins/iscsi.csi.k8s.io/targetd-negative",
		VolumeContext: pv.Spec.CSI.VolumeAttributes, PublishContext: attachment.Status.AttachmentMetadata,
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err == nil || !strings.Contains(status.Convert(err).Message(), "iSCSI device serial does not match volume UUID") {
		return fmt.Errorf("wrong serial was not rejected at the device check: %v", err)
	}
	fmt.Println("Wrong serial rejected before formatting or mounting")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
