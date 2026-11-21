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

// controller-handoff exercises single-writer ownership on a live targetd controller.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/iscsi"
	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/targetd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const pool = "vg-targetd-e2e"

func checkExport(ctx context.Context, backend *targetd.Client, uuid, initiator string, lun int) error {
	rows, err := backend.Exports(ctx)
	if err != nil {
		return err
	}
	matches := 0
	for _, e := range rows {
		if e.VolumeUUID != uuid {
			continue
		}
		if e.Pool != pool || e.InitiatorWWN != initiator || e.LUN != lun {
			return fmt.Errorf("unexpected mapping for volume %s: %+v", uuid, e)
		}
		matches++
	}
	if (initiator == "" && matches != 0) || (initiator != "" && matches != 1) {
		return fmt.Errorf("volume %s has %d matching exports for initiator %q", uuid, matches, initiator)
	}
	return nil
}

func run() (err error) {
	if len(os.Args) != 3 {
		return errors.New("expected the two fixture node IDs")
	}
	firstNode, firstIQN, err := iscsi.DecodeNodeID(os.Args[1])
	if err != nil {
		return err
	}
	secondNode, secondIQN, err := iscsi.DecodeNodeID(os.Args[2])
	if err != nil || firstNode == secondNode || firstIQN == secondIQN {
		return errors.New("handoff requires two distinct node names and IQNs")
	}
	data, err := os.ReadFile("/etc/targetd/config.json")
	if err != nil {
		return err
	}
	var cfg iscsi.TargetdConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if cfg.BackendID != "targetd-e2e" || cfg.Namespace != "targetd-e2e" || len(cfg.Pools) != 1 || cfg.Pools[0] != pool {
		return errors.New("handoff requires the dedicated targetd fixture backend")
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return err
	}
	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return err
	}
	backend, err := targetd.New(targetd.Config{Endpoint: cfg.Endpoint, CA: ca, Username: cfg.Username, Password: strings.TrimSuffix(string(password), "\n")})
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient("unix:///csi/csi.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	c := csi.NewControllerClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	capability := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}
	created, err := c.CreateVolume(ctx, &csi.CreateVolumeRequest{Name: "handoff-" + secondNode, Parameters: map[string]string{"pool": pool}, CapacityRange: &csi.CapacityRange{RequiredBytes: 64 << 20}, VolumeCapabilities: []*csi.VolumeCapability{capability}})
	if err != nil {
		return err
	}
	id := created.GetVolume().GetVolumeId()
	if id == "" {
		return errors.New("controller returned an empty volume ID")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, detachErr := c.ControllerUnpublishVolume(cleanup, &csi.ControllerUnpublishVolumeRequest{VolumeId: id}); detachErr != nil {
			err = errors.Join(err, detachErr)
			return
		}
		_, deleteErr := c.DeleteVolume(cleanup, &csi.DeleteVolumeRequest{VolumeId: id})
		err = errors.Join(err, deleteErr)
	}()
	req := &csi.ControllerPublishVolumeRequest{VolumeId: id, VolumeContext: created.Volume.VolumeContext, VolumeCapability: capability, NodeId: os.Args[1]}
	first, err := c.ControllerPublishVolume(ctx, req)
	if err != nil {
		return err
	}
	uuid := first.GetPublishContext()["volumeUUID"]
	lun, err := strconv.Atoi(first.GetPublishContext()["lun"])
	if err != nil || uuid == "" {
		return errors.New("incomplete first attachment")
	}
	req.NodeId = os.Args[2]
	if _, err = c.ControllerPublishVolume(ctx, req); status.Code(err) != codes.FailedPrecondition {
		return fmt.Errorf("second writer returned %v, want FailedPrecondition", err)
	}
	if err := checkExport(ctx, backend, uuid, firstIQN, lun); err != nil {
		return err
	}
	if _, err := c.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: os.Args[1]}); err != nil {
		return err
	}
	if err := checkExport(ctx, backend, uuid, "", -1); err != nil {
		return err
	}
	second, err := c.ControllerPublishVolume(ctx, req)
	if err != nil {
		return err
	}
	lun, err = strconv.Atoi(second.GetPublishContext()["lun"])
	if err != nil || second.GetPublishContext()["volumeUUID"] != uuid {
		return errors.New("volume identity changed during handoff")
	}
	return checkExport(ctx, backend, uuid, secondIQN, lun)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "controller handoff:", err)
		os.Exit(1)
	}
	fmt.Println("Live targetd export inventory confirmed writer exclusion, detach and handoff to the second registered IQN.")
}
