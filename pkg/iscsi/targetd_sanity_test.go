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
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// Controller specs need a node identity and call unpublish during cleanup.
type sanityNode struct{ csi.UnimplementedNodeServer }

func (sanityNode) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: EncodeNodeID("node", "iqn.1993-08.org.debian:sanity")}, nil
}

func (sanityNode) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (sanityNode) NodeUnpublishVolume(context.Context, *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// make sanity-test runs the upstream CSI suite against the production controller RPCs.
func TestTargetdControllerSanity(t *testing.T) {
	bin := os.Getenv("CSI_SANITY_BIN")
	if bin == "" {
		t.Skip("make sanity-test supplies csi-sanity")
	}
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	settings, err := json.Marshal(TargetdConfig{
		BackendID: "sanity", Namespace: "test", Endpoint: "https://storage.example/targetrpc",
		Username: "test", TargetIQN: "iqn.2026-01.example:target",
		Portals: []string{"192.0.2.1"}, Pools: []string{"data"}, PasswordFile: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, settings, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "controller.sock")
	endpoint := "unix://" + socket
	controller, err := NewTargetdControllerDriver(endpoint, config)
	if err != nil {
		t.Fatal(err)
	}
	fixture, backend := testController()
	controller.targetd.api = backend
	controller.targetd.barrier = fixture.barrier
	controller.targetd.checkNode = fixture.checkNode
	controller.targetd.checkDetach = fixture.checkDetach

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, NewDefaultIdentityServer(controller))
	csi.RegisterControllerServer(server, NewControllerServer(controller))
	csi.RegisterNodeServer(server, sanityNode{})
	go func() {
		if err := server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(server.Stop)
	cmd := exec.Command(bin,
		"-csi.endpoint="+endpoint,
		"-csi.mountdir="+filepath.Join(dir, "mount"), "-csi.stagingdir="+filepath.Join(dir, "stage"),
		"-ginkgo.focus=\\[Controller Server\\]", "-ginkgo.no-color", "-ginkgo.timeout=1m",
	)
	output, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(output, []byte("Ran 0 of")) {
		t.Fatalf("controller sanity: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
