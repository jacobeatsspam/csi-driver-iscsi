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
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/targetd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeTargetd struct {
	volumes                  map[string]targetd.Volume
	exports                  []targetd.Export
	createCalls, deleteCalls int
	createTimeout            bool
	deferExport              bool
	queuedExport             func()
}

func (f *fakeTargetd) Pools(context.Context) ([]targetd.Pool, error) {
	return []targetd.Pool{{Name: "data", Type: "block", FreeSize: 1 << 40}, {Name: "other", Type: "block", FreeSize: 1 << 40}}, nil
}
func (f *fakeTargetd) Volumes(_ context.Context, pool string) ([]targetd.Volume, error) {
	vols := []targetd.Volume{}
	for key, v := range f.volumes {
		if strings.HasPrefix(key, pool+"/") {
			vols = append(vols, v)
		}
	}
	return vols, nil
}
func (f *fakeTargetd) CreateVolume(_ context.Context, pool, name string, size int64) error {
	f.createCalls++
	size = (size + (4 << 20) - 1) / (4 << 20) * (4 << 20)
	f.volumes[pool+"/"+name] = targetd.Volume{Name: name, Size: size, UUID: "abcdef-abcd-abcd-abcd-abcd-abcd-abcdef"}
	if f.createTimeout {
		f.createTimeout = false
		return context.DeadlineExceeded
	}
	return nil
}
func (f *fakeTargetd) DeleteVolume(_ context.Context, pool, name string) error {
	f.deleteCalls++
	delete(f.volumes, pool+"/"+name)
	return nil
}
func (f *fakeTargetd) Exports(context.Context) ([]targetd.Export, error) {
	return append([]targetd.Export(nil), f.exports...), nil
}
func (f *fakeTargetd) CreateExport(_ context.Context, pool, name, initiator string, lun int) error {
	v := f.volumes[pool+"/"+name]
	add := func() {
		f.exports = append(f.exports, targetd.Export{Pool: pool, VolumeName: name, VolumeUUID: v.UUID, InitiatorWWN: initiator, LUN: lun})
	}
	if f.deferExport {
		f.queuedExport = add
		return context.DeadlineExceeded
	}
	add()
	return nil
}
func (f *fakeTargetd) DeleteExport(_ context.Context, pool, name, initiator string) error {
	for i, e := range f.exports {
		if e.Pool == pool && e.VolumeName == name && e.InitiatorWWN == initiator {
			f.exports = append(f.exports[:i], f.exports[i+1:]...)
			break
		}
	}
	return nil
}

func testController() (*targetdController, *fakeTargetd) {
	f := &fakeTargetd{volumes: map[string]targetd.Volume{}}
	barrier, _ := newFakeBarrier()
	return &targetdController{api: f, backendID: "backend", namespace: "cluster", iqn: "iqn.2026-01.test.example:target", pools: []string{"data", "other"}, portals: []string{"127.0.0.1:3260"}, checkNode: func(context.Context, string, string, string) error { return nil }, checkDetach: func(context.Context, string, string) error { return nil }, barrier: barrier}, f
}
func testCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}
}
func createRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{Name: "pvc-test", CapacityRange: &csi.CapacityRange{RequiredBytes: 64 << 20}, VolumeCapabilities: []*csi.VolumeCapability{testCapability()}, Parameters: map[string]string{"pool": "data"}}
}

func TestTargetdLifecycleAndReplacement(t *testing.T) {
	ctx := context.Background()
	c, f := testController()
	created, err := c.createVolume(ctx, createRequest())
	if err != nil {
		t.Fatal(err)
	}
	id := created.Volume.VolumeId
	if len(id) > 128 || strings.ContainsAny(id, "/.") {
		t.Fatalf("unsafe volume ID %q", id)
	}
	other, err := c.createVolume(ctx, createRequest())
	if err != nil || other.Volume.VolumeId != id || f.createCalls != 1 {
		t.Fatalf("idempotent create: %v, calls %d", err, f.createCalls)
	}
	node1 := EncodeNodeID("node1", "iqn.1993-08.org.debian:node1")
	node2 := EncodeNodeID("node2", "iqn.1993-08.org.debian:node2")
	pub := &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: node1, VolumeCapability: testCapability()}
	first, err := c.publishVolume(ctx, pub)
	if err != nil || first.GetPublishContext()["lun"] != "0" {
		t.Fatalf("publish: %v %v", first, err)
	}
	if _, err := c.publishVolume(ctx, pub); err != nil || len(f.exports) != 1 {
		t.Fatalf("repeat publish: %v", err)
	}
	pub.NodeId = node2
	if _, err := c.publishVolume(ctx, pub); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second writer: %v", err)
	}
	if _, err := c.deleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete exported volume: %v", err)
	}
	if _, err := c.unpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: node1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.publishVolume(ctx, pub); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	if _, err := c.unpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	parsed, err := c.parseID(id)
	if err != nil {
		t.Fatal(err)
	}
	replacement := f.volumes[parsed.pool+"/"+parsed.name]
	replacement.UUID = "replacement"
	f.volumes[parsed.pool+"/"+parsed.name] = replacement
	if _, err := c.deleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); status.Code(err) != codes.FailedPrecondition || f.deleteCalls != 0 {
		t.Fatalf("replacement was not protected: %v", err)
	}
	delete(f.volumes, parsed.pool+"/"+parsed.name)
	if _, err := c.deleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestTargetdCreateRetryAndCapacity(t *testing.T) {
	ctx := context.Background()
	c, f := testController()
	f.createTimeout = true
	if _, err := c.createVolume(ctx, createRequest()); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected ambiguous timeout: %v", err)
	}
	if _, err := c.createVolume(ctx, createRequest()); status.Code(err) != codes.FailedPrecondition || f.createCalls != 1 {
		t.Fatalf("ambiguous mutation must block retry: %v, calls %d", err, f.createCalls)
	}
	barrier := c.barrier.(*kubernetesBarrier)
	maps, _ := barrier.configMaps()
	fakeMaps := maps.(*fakeBarrierMaps)
	delete(fakeMaps.config.Data, targetdBarrierKey) // Simulate operator recovery after targetd is stopped.
	if _, err := c.createVolume(ctx, createRequest()); err != nil || f.createCalls != 1 {
		t.Fatalf("reconcile retry: %v, calls %d", err, f.createCalls)
	}
	req := createRequest()
	req.Parameters["pool"] = "other"
	if _, err := c.createVolume(ctx, req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different pool: %v", err)
	}
	req = createRequest()
	req.Name = "limit"
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: (4 << 20) + 1, LimitBytes: (4 << 20) + 1}
	if _, err := c.createVolume(ctx, req); status.Code(err) != codes.OutOfRange || f.deleteCalls != 1 {
		t.Fatalf("rounded capacity: %v, deletes %d", err, f.deleteCalls)
	}
}

func TestTargetdFilesystemIdentity(t *testing.T) {
	c, _ := testController()
	req := createRequest()
	req.VolumeCapabilities[0].GetMount().FsType = ""
	first, err := c.createVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.VolumeCapabilities[0].GetMount().FsType = "ext4"
	again, err := c.createVolume(context.Background(), req)
	if err != nil || first.Volume.VolumeId != again.Volume.VolumeId {
		t.Fatalf("default filesystem must match ext4: %v", err)
	}
	req.VolumeCapabilities[0].GetMount().FsType = "xfs"
	if _, err := c.createVolume(context.Background(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed filesystem: %v", err)
	}
}

func TestTargetdCapacityWithoutMinimum(t *testing.T) {
	ctx := context.Background()
	c, f := testController()
	req := createRequest()
	first, err := c.createVolume(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []*csi.CapacityRange{nil, {LimitBytes: 256 << 20}} {
		req.CapacityRange = capacity
		again, err := c.createVolume(ctx, req)
		if err != nil || again.GetVolume().GetVolumeId() != first.Volume.VolumeId || f.createCalls != 1 {
			t.Fatalf("compatible retry: volume %v, error %v, creates %d", again, err, f.createCalls)
		}
	}
	req.Name = "limit-only"
	req.CapacityRange = &csi.CapacityRange{LimitBytes: 7 << 20}
	small, err := c.createVolume(ctx, req)
	if err != nil || small.GetVolume().GetCapacityBytes() != 4<<20 {
		t.Fatalf("limit-only volume: %v, %v", small, err)
	}
	req.Name = "small-xfs"
	req.VolumeCapabilities[0].GetMount().FsType = "xfs"
	if _, err := c.createVolume(ctx, req); status.Code(err) != codes.OutOfRange || f.createCalls != 2 {
		t.Fatalf("small XFS limit: %v, creates %d", err, f.createCalls)
	}
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: 16 << 20}
	xfs, err := c.createVolume(ctx, req)
	if err != nil || xfs.GetVolume().GetCapacityBytes() != 300<<20 {
		t.Fatalf("XFS minimum size: %v, %v", xfs, err)
	}
}

func TestTargetdVolumeContext(t *testing.T) {
	ctx := context.Background()
	c, f := testController()
	c.portals = append(c.portals, "192.0.2.1:3260")
	created, err := c.createVolume(ctx, createRequest())
	if err != nil {
		t.Fatal(err)
	}
	vol := created.Volume
	pub := &csi.ControllerPublishVolumeRequest{VolumeId: vol.VolumeId, NodeId: EncodeNodeID("node", "iqn.1993-08.org.debian:node"), VolumeCapability: testCapability(), VolumeContext: vol.VolumeContext}
	validate := &csi.ValidateVolumeCapabilitiesRequest{VolumeId: vol.VolumeId, VolumeCapabilities: []*csi.VolumeCapability{testCapability()}, VolumeContext: vol.VolumeContext}
	confirmed, err := c.validateVolume(ctx, validate)
	if err != nil || confirmed.GetConfirmed() == nil {
		t.Fatalf("valid volume: %v, %v", confirmed, err)
	}
	validate.Parameters = map[string]string{"pool": "other"}
	confirmed, err = c.validateVolume(ctx, validate)
	if err != nil || confirmed.GetConfirmed() != nil {
		t.Fatalf("confirmed mismatched pool: %v, %v", confirmed, err)
	}
	validate.Parameters = nil
	vol.VolumeContext["portals"] = `["192.0.2.2:3260"]`
	if _, err := c.publishVolume(ctx, pub); status.Code(err) != codes.FailedPrecondition || len(f.exports) != 0 {
		t.Fatalf("published with conflicting portal: %v, exports %v", err, f.exports)
	}
	confirmed, err = c.validateVolume(ctx, validate)
	if err != nil || confirmed.GetConfirmed() != nil {
		t.Fatalf("confirmed mismatched portal: %v, %v", confirmed, err)
	}
	pub.VolumeContext = nil
	attached, err := c.publishVolume(ctx, pub)
	if err != nil || attached.GetPublishContext()["portals"] != `["192.0.2.1:3260"]` {
		t.Fatalf("secondary portals missing from attachment: %v, %v", attached, err)
	}
	conflict := f.exports[0]
	conflict.VolumeName = "other"
	f.exports = append(f.exports, conflict)
	if _, err := c.publishVolume(ctx, pub); status.Code(err) != codes.FailedPrecondition || len(f.exports) != 2 {
		t.Fatalf("accepted conflicting mappings: %v, %v", err, f.exports)
	}
}

func TestTargetdConcurrentCreate(t *testing.T) {
	c, f := testController()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := c.createVolume(context.Background(), createRequest()); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.createCalls != 1 {
		t.Fatalf("created %d times", f.createCalls)
	}
}

func TestTargetdDelayedExportBlocksAfterRestart(t *testing.T) {
	ctx := context.Background()
	c, f := testController()
	created, err := c.createVolume(ctx, createRequest())
	if err != nil {
		t.Fatal(err)
	}
	f.deferExport = true
	oldNode := EncodeNodeID("old-node", "iqn.1993-08.org.debian:old")
	newNode := EncodeNodeID("new-node", "iqn.1993-08.org.debian:new")
	request := &csi.ControllerPublishVolumeRequest{VolumeId: created.Volume.VolumeId, NodeId: oldNode, VolumeCapability: testCapability()}
	if _, err := c.publishVolume(ctx, request); status.Code(err) != codes.DeadlineExceeded || f.queuedExport == nil {
		t.Fatalf("expected queued export timeout: %v", err)
	}
	// A new controller sees the same durable marker while the backend is still queued.
	restarted, _ := testController()
	restarted.api, restarted.barrier = f, c.barrier
	if _, err := restarted.unpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: created.Volume.VolumeId, NodeId: oldNode}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unpublish crossed pending export: %v", err)
	}
	request.NodeId = newNode
	if _, err := restarted.publishVolume(ctx, request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second publish crossed pending export: %v", err)
	}
	f.queuedExport()
	if _, err := restarted.publishVolume(ctx, request); status.Code(err) != codes.FailedPrecondition || len(f.exports) != 1 {
		t.Fatalf("late export did not remain blocked: %v", err)
	}
}

func TestTargetdNodeRegistration(t *testing.T) {
	iqn := "iqn.1993-08.org.debian:node1"
	id := EncodeNodeID("node1", iqn)
	nodes := []storagev1.CSINode{{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: driverName, NodeID: id}}}}}
	if err := uniqueRegisteredInitiator(nodes, id, "node1", iqn); err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "node2"}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: driverName, NodeID: EncodeNodeID("node2", iqn)}}}})
	if err := uniqueRegisteredInitiator(nodes, id, "node1", iqn); err == nil {
		t.Fatal("duplicate initiator accepted")
	}
	nodes[1].Spec.Drivers[0].NodeID = "node2"
	if err := uniqueRegisteredInitiator(nodes, id, "node1", iqn); err == nil {
		t.Fatal("legacy node identity accepted")
	}
}

func TestTargetdStaleDetach(t *testing.T) {
	iqn := "iqn.1993-08.org.debian:node1"
	nodes := []storagev1.CSINode{{ObjectMeta: metav1.ObjectMeta{Name: "replacement"}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: driverName, NodeID: EncodeNodeID("replacement", iqn)}}}}}
	if err := checkDetachRegistration(nodes, "old-node", iqn); err == nil {
		t.Fatal("stale detach accepted an initiator reused by another node")
	}
	if err := checkDetachRegistration(nil, "old-node", iqn); err != nil {
		t.Fatalf("cannot clean up removed node: %v", err)
	}
	c, f := testController()
	created, err := c.createVolume(context.Background(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	newNodeID := EncodeNodeID("replacement", iqn)
	if _, err := c.publishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{VolumeId: created.Volume.VolumeId, NodeId: newNodeID, VolumeCapability: testCapability()}); err != nil {
		t.Fatal(err)
	}
	c.checkDetach = func(_ context.Context, node, initiator string) error {
		if err := checkDetachRegistration(nodes, node, initiator); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil
	}
	_, err = c.unpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: created.Volume.VolumeId, NodeId: EncodeNodeID("old-node", iqn)})
	if status.Code(err) != codes.FailedPrecondition || len(f.exports) != 1 {
		t.Fatalf("stale detach removed replacement export: %v", err)
	}
}
