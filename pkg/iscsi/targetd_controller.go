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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/targetd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedstoragev1 "k8s.io/client-go/kubernetes/typed/storage/v1"
	"k8s.io/client-go/rest"
)

type targetdAPI interface {
	Pools(context.Context) ([]targetd.Pool, error)
	Volumes(context.Context, string) ([]targetd.Volume, error)
	CreateVolume(context.Context, string, string, int64) error
	DeleteVolume(context.Context, string, string) error
	Exports(context.Context) ([]targetd.Export, error)
	CreateExport(context.Context, string, string, string, int) error
	DeleteExport(context.Context, string, string, string) error
}

type targetdController struct {
	api                       targetdAPI
	backendID, namespace, iqn string
	pools, portals            []string
	checkNode                 func(context.Context, string, string, string) error
	checkDetach               func(context.Context, string, string) error
	barrier                   mutationBarrier
	mu                        sync.Mutex
}

type targetdVolumeID struct{ pool, name, uuid string }

func (c *targetdController) checkBarrier(ctx context.Context) error {
	if c.barrier == nil {
		return status.Error(codes.FailedPrecondition, "mutation barrier is not configured")
	}
	return c.barrier.Check(ctx)
}

func (c *targetdController) mutate(ctx context.Context, operation string, fn func(context.Context) error) error {
	err := c.barrier.Mutate(ctx, operation, fn)
	if _, ok := status.FromError(err); ok {
		return err
	}
	return backendError(ctx, err)
}

var safeTargetdID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
var errNodeNotRegistered = errors.New("node identity is not registered")

func uniqueRegisteredInitiator(nodes []storagev1.CSINode, requestedID, node, initiator string) error {
	found := false
	for _, entry := range nodes {
		for _, driver := range entry.Spec.Drivers {
			if driver.Name != driverName {
				continue
			}
			registeredNode, registeredIQN, err := DecodeNodeID(driver.NodeID)
			if err != nil {
				return errors.New("node identity has not been upgraded")
			}
			if registeredNode != entry.Name {
				return errors.New("node identity does not match registration")
			}
			if registeredIQN == initiator && registeredNode != node {
				return errors.New("initiator is registered by multiple nodes")
			}
			if registeredNode == node && driver.NodeID == requestedID {
				found = true
			}
		}
	}
	if !found {
		return errNodeNotRegistered
	}
	return nil
}

func registeredNodes(ctx context.Context) ([]storagev1.CSINode, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, status.Error(codes.Unavailable, "cannot access node registry")
	}
	client, err := typedstoragev1.NewForConfig(config)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "cannot access node registry")
	}
	nodes, err := client.CSINodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, backendError(ctx, err)
	}
	return nodes.Items, nil
}

func checkKubernetesNode(ctx context.Context, id, node, initiator string) error {
	nodes, err := registeredNodes(ctx)
	if err != nil {
		return err
	}
	if err := uniqueRegisteredInitiator(nodes, id, node, initiator); err != nil {
		if errors.Is(err, errNodeNotRegistered) {
			return status.Error(codes.NotFound, err.Error())
		}
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}

func checkDetachRegistration(nodes []storagev1.CSINode, node, initiator string) error {
	for _, entry := range nodes {
		for _, driver := range entry.Spec.Drivers {
			if driver.Name != driverName {
				continue
			}
			registeredNode, registeredIQN, err := DecodeNodeID(driver.NodeID)
			if err != nil || registeredNode != entry.Name {
				return errors.New("node identity has not been upgraded")
			}
			if registeredIQN == initiator && registeredNode != node {
				return errors.New("initiator is registered by a different node")
			}
		}
	}
	return nil
}

func checkKubernetesDetach(ctx context.Context, node, initiator string) error {
	nodes, err := registeredNodes(ctx)
	if err != nil {
		return err
	}
	if err := checkDetachRegistration(nodes, node, initiator); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func poolHash(s string) string   { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:4]) }
func policyHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:8]) }

// A name identifies the request and its immutable filesystem policy without
// depending on controller memory after a successful but unacknowledged create.
func (c *targetdController) volumeName(requestName string, caps []*csi.VolumeCapability) (string, error) {
	if err := validateCapabilities(caps); err != nil {
		return "", err
	}
	return "csitd_" + shortHash(c.namespace) + "_" + shortHash(requestName) + "_" + policyHash(filesystem(caps[0])), nil
}

func filesystem(cap *csi.VolumeCapability) string {
	fs := cap.GetMount().GetFsType()
	if fs == "" {
		return "ext4"
	}
	return fs
}

func validateCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range caps {
		if cap == nil || cap.GetMount() == nil || cap.GetAccessMode() == nil {
			return status.Error(codes.InvalidArgument, "filesystem mount and access mode are required")
		}
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return status.Error(codes.InvalidArgument, "only SINGLE_NODE_WRITER is supported")
		}
		fs := cap.GetMount().GetFsType()
		if fs != "" && fs != "ext4" && fs != "xfs" {
			return status.Error(codes.InvalidArgument, "unsupported filesystem")
		}
		if filesystem(cap) != filesystem(caps[0]) {
			return status.Error(codes.InvalidArgument, "filesystem capabilities conflict")
		}
	}
	return nil
}

func (c *targetdController) id(pool, name, uuid string) (string, error) {
	if !safeTargetdID.MatchString(uuid) {
		return "", errors.New("invalid backend volume UUID")
	}
	id := "td1_" + shortHash(c.backendID) + "_" + poolHash(pool) + "_" + name + "_" + uuid
	if len(id) > 128 {
		return "", errors.New("backend volume identity exceeds CSI limit")
	}
	return id, nil
}

func (c *targetdController) parseID(id string) (targetdVolumeID, error) {
	parts := strings.Split(id, "_")
	if len(parts) != 8 || parts[0] != "td1" || parts[1] != shortHash(c.backendID) || parts[3] != "csitd" || parts[4] != shortHash(c.namespace) || !safeTargetdID.MatchString(parts[7]) {
		return targetdVolumeID{}, status.Error(codes.InvalidArgument, "invalid targetd volume ID")
	}
	for _, pool := range c.pools {
		if parts[2] == poolHash(pool) {
			name := strings.Join(parts[3:7], "_")
			if len(parts[5]) != 16 || len(parts[6]) != 16 || len(id) > 128 {
				break
			}
			if _, err := hex.DecodeString(parts[5]); err != nil {
				break
			}
			if _, err := hex.DecodeString(parts[6]); err != nil {
				break
			}
			return targetdVolumeID{pool: pool, name: name, uuid: parts[7]}, nil
		}
	}
	return targetdVolumeID{}, status.Error(codes.InvalidArgument, "volume ID does not match configured backend")
}

func backendError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "targetd request timed out")
	}
	var rpcErr *targetd.Error
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case -50:
			return status.Error(codes.AlreadyExists, "targetd name conflict")
		case -1000:
			return status.Error(codes.ResourceExhausted, "targetd has no available LUN")
		case -110, -152:
			return status.Error(codes.FailedPrecondition, "targetd pool is unavailable")
		case -103, -151:
			return status.Error(codes.NotFound, "targetd resource not found")
		}
	}
	var httpErr *targetd.HTTPError
	if errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403) {
		return status.Error(codes.PermissionDenied, "targetd authentication failed")
	}
	return status.Error(codes.Unavailable, "targetd request failed")
}

func (c *targetdController) volume(ctx context.Context, id targetdVolumeID) (*targetd.Volume, error) {
	volumes, err := c.api.Volumes(ctx, id.pool)
	if err != nil {
		return nil, backendError(ctx, err)
	}
	for _, vol := range volumes {
		if vol.Name == id.name {
			if vol.UUID != id.uuid {
				return nil, status.Error(codes.FailedPrecondition, "volume was replaced")
			}
			return &vol, nil
		}
	}
	return nil, nil
}

func (c *targetdController) volumeContext() map[string]string {
	portals, _ := json.Marshal(c.portals[1:])
	return map[string]string{"targetPortal": c.portals[0], "iqn": c.iqn, "portals": string(portals)}
}

func matchesVolumeContext(configured, supplied map[string]string) bool {
	for key, expected := range configured {
		if value, ok := supplied[key]; ok && value != expected {
			return false
		}
	}
	return true
}

func validStorageParameters(params map[string]string) bool {
	for key := range params {
		if key != "pool" && !strings.HasPrefix(key, "csi.storage.k8s.io/") {
			return false
		}
	}
	return true
}

func (c *targetdController) createVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if req.GetVolumeContentSource() != nil || req.GetAccessibilityRequirements() != nil || len(req.GetMutableParameters()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "content sources, topology and volume modification are unsupported")
	}
	name, err := c.volumeName(req.GetName(), req.GetVolumeCapabilities())
	if err != nil {
		return nil, err
	}
	params := req.GetParameters()
	if !validStorageParameters(params) {
		return nil, status.Error(codes.InvalidArgument, "unsupported storage parameter")
	}
	pool := params["pool"]
	if pool == "" && len(c.pools) == 1 {
		pool = c.pools[0]
	}
	if !slices.Contains(c.pools, pool) {
		return nil, status.Error(codes.InvalidArgument, "pool is not configured")
	}
	required, limit := req.GetCapacityRange().GetRequiredBytes(), req.GetCapacityRange().GetLimitBytes()
	if required < 0 || limit < 0 || (limit > 0 && required > limit) || (req.GetCapacityRange() != nil && required == 0 && limit == 0) {
		return nil, status.Error(codes.InvalidArgument, "invalid capacity range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkBarrier(ctx); err != nil {
		return nil, err
	}
	requestPrefix := name[:strings.LastIndex(name, "_")+1]
	var found *targetd.Volume
	for _, candidatePool := range c.pools {
		vols, err := c.api.Volumes(ctx, candidatePool)
		if err != nil {
			return nil, backendError(ctx, err)
		}
		for _, v := range vols {
			if strings.HasPrefix(v.Name, requestPrefix) {
				if found != nil || candidatePool != pool || v.Name != name || v.Size < required || (limit > 0 && v.Size > limit) {
					return nil, status.Error(codes.AlreadyExists, "volume name exists with incompatible parameters")
				}
				copy := v
				found = &copy
			}
		}
	}
	if found == nil {
		size := max(required, 1<<20)
		if filesystem(req.GetVolumeCapabilities()[0]) == "xfs" {
			// mkfs.xfs requires at least 300 MiB with its default format.
			size = max(size, 300<<20)
		}
		if limit > 0 && size > limit {
			return nil, status.Error(codes.OutOfRange, "capacity limit is too small for the filesystem")
		}
		pools, err := c.api.Pools(ctx)
		if err != nil {
			return nil, backendError(ctx, err)
		}
		available := false
		for _, candidate := range pools {
			if candidate.Name == pool && candidate.Type == "block" {
				available = true
				if candidate.FreeSize < size {
					return nil, status.Error(codes.ResourceExhausted, "insufficient pool capacity")
				}
				if required == 0 {
					// Prefer 1 GiB. Powers of two avoid unnecessary rounding by LVM.
					preferred := int64(1 << 30)
					for preferred > candidate.FreeSize || (limit > 0 && preferred > limit) {
						preferred /= 2
					}
					size = max(size, preferred)
				}
			}
		}
		if !available {
			return nil, status.Error(codes.FailedPrecondition, "configured pool is unavailable")
		}
		if err := c.mutate(ctx, fmt.Sprintf("create-volume %s/%s", pool, name), func(ctx context.Context) error { return c.api.CreateVolume(ctx, pool, name, size) }); err != nil {
			return nil, err
		}
		vols, err := c.api.Volumes(ctx, pool)
		if err != nil {
			return nil, backendError(ctx, err)
		}
		for _, v := range vols {
			if v.Name == name {
				copy := v
				found = &copy
				break
			}
		}
		if found == nil {
			return nil, status.Error(codes.Unavailable, "created volume is not visible")
		}
		if found.Size < required || (limit > 0 && found.Size > limit) {
			if !safeTargetdID.MatchString(found.UUID) {
				return nil, status.Error(codes.FailedPrecondition, "cannot verify newly allocated volume")
			}
			current, err := c.volume(ctx, targetdVolumeID{pool: pool, name: name, uuid: found.UUID})
			if err != nil {
				return nil, err
			}
			if current == nil {
				return nil, status.Error(codes.Unavailable, "allocated volume disappeared")
			}
			exports, err := c.api.Exports(ctx)
			if err != nil {
				return nil, backendError(ctx, err)
			}
			for _, export := range exports {
				if export.Pool == pool && export.VolumeName == name {
					return nil, status.Error(codes.FailedPrecondition, "allocated volume is exported")
				}
			}
			if err := c.mutate(ctx, fmt.Sprintf("delete-volume %s/%s", pool, name), func(ctx context.Context) error { return c.api.DeleteVolume(ctx, pool, name) }); err != nil {
				return nil, err
			}
			return nil, status.Error(codes.OutOfRange, "allocated capacity is outside the requested range")
		}
	}
	id, err := c.id(pool, name, found.UUID)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid targetd volume identity")
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{VolumeId: id, CapacityBytes: found.Size, VolumeContext: c.volumeContext()}}, nil
}

func (c *targetdController) deleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if !strings.HasPrefix(req.GetVolumeId(), "td1_") {
		return &csi.DeleteVolumeResponse{}, nil
	}
	id, err := c.parseID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkBarrier(ctx); err != nil {
		return nil, err
	}
	vol, err := c.volume(ctx, id)
	if err != nil {
		return nil, err
	}
	if vol == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}
	exports, err := c.api.Exports(ctx)
	if err != nil {
		return nil, backendError(ctx, err)
	}
	for _, e := range exports {
		if e.Pool == id.pool && e.VolumeName == id.name {
			return nil, status.Error(codes.FailedPrecondition, "volume is exported")
		}
	}
	if err := c.mutate(ctx, fmt.Sprintf("delete-volume %s/%s", id.pool, id.name), func(ctx context.Context) error { return c.api.DeleteVolume(ctx, id.pool, id.name) }); err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func checkStaticVolume(id string, attrs map[string]string) error {
	if !validVolumeID(id) || attrs["targetPortal"] == "" || attrs["iqn"] == "" || attrs["lun"] == "" {
		return status.Error(codes.NotFound, "unknown static volume")
	}
	return nil
}

func (c *targetdController) publishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetNodeId() == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume, node and capability are required")
	}
	if !strings.HasPrefix(req.GetVolumeId(), "td1_") {
		if err := checkStaticVolume(req.GetVolumeId(), req.GetVolumeContext()); err != nil {
			return nil, err
		}
		return &csi.ControllerPublishVolumeResponse{}, nil
	}
	if req.GetReadonly() {
		return nil, status.Error(codes.InvalidArgument, "read-only controller publish is unsupported")
	}
	if err := validateCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, err
	}
	id, err := c.parseID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(id.name, "_"+policyHash(filesystem(req.GetVolumeCapability()))) {
		return nil, status.Error(codes.FailedPrecondition, "volume filesystem does not match capability")
	}
	connection := c.volumeContext()
	if !matchesVolumeContext(connection, req.GetVolumeContext()) {
		return nil, status.Error(codes.FailedPrecondition, "volume context does not match backend")
	}
	node, initiator, err := DecodeNodeID(req.GetNodeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "unknown node initiator")
	}
	if c.checkNode == nil {
		return nil, status.Error(codes.FailedPrecondition, "node registry is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkBarrier(ctx); err != nil {
		return nil, err
	}
	if err := c.checkNode(ctx, req.GetNodeId(), node, initiator); err != nil {
		return nil, err
	}
	vol, err := c.volume(ctx, id)
	if err != nil {
		return nil, err
	}
	if vol == nil {
		return nil, status.Error(codes.NotFound, "volume does not exist")
	}
	exports, err := c.api.Exports(ctx)
	if err != nil {
		return nil, backendError(ctx, err)
	}
	used := make(map[int]bool)
	lun := -1
	for _, e := range exports {
		if e.InitiatorWWN == initiator {
			if used[e.LUN] {
				return nil, status.Error(codes.FailedPrecondition, "initiator has conflicting LUN mappings")
			}
			used[e.LUN] = true
		}
		if e.Pool == id.pool && e.VolumeName == id.name {
			if e.VolumeUUID != id.uuid || e.InitiatorWWN != initiator {
				return nil, status.Error(codes.FailedPrecondition, "volume is exported to a different node or replacement")
			}
			if lun != -1 && lun != e.LUN {
				return nil, status.Error(codes.FailedPrecondition, "multiple LUNs found for volume")
			}
			lun = e.LUN
		}
	}
	if lun == -1 {
		for i := 0; i <= 255; i++ {
			if !used[i] {
				lun = i
				break
			}
		}
		if lun < 0 {
			return nil, status.Error(codes.ResourceExhausted, "no free LUN")
		}
		if err := c.mutate(ctx, fmt.Sprintf("create-export %s/%s initiator=%s lun=%d", id.pool, id.name, initiator, lun), func(ctx context.Context) error { return c.api.CreateExport(ctx, id.pool, id.name, initiator, lun) }); err != nil {
			return nil, err
		}
	}
	connection["lun"] = strconv.Itoa(lun)
	connection["initiatorName"] = initiator
	connection["volumeUUID"] = id.uuid
	return &csi.ControllerPublishVolumeResponse{PublishContext: connection}, nil
}

func (c *targetdController) unpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if !strings.HasPrefix(req.GetVolumeId(), "td1_") {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	id, err := c.parseID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkBarrier(ctx); err != nil {
		return nil, err
	}
	initiator := ""
	if req.GetNodeId() != "" {
		var node string
		node, initiator, err = DecodeNodeID(req.GetNodeId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "unknown node initiator")
		}
		if c.checkDetach == nil {
			return nil, status.Error(codes.FailedPrecondition, "node registry is not configured")
		}
		if err := c.checkDetach(ctx, node, initiator); err != nil {
			return nil, err
		}
	}
	vol, err := c.volume(ctx, id)
	if err != nil {
		return nil, err
	}
	if vol == nil {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	exports, err := c.api.Exports(ctx)
	if err != nil {
		return nil, backendError(ctx, err)
	}
	for _, e := range exports {
		if e.Pool != id.pool || e.VolumeName != id.name || (initiator != "" && e.InitiatorWWN != initiator) {
			continue
		}
		if e.VolumeUUID != id.uuid {
			return nil, status.Error(codes.FailedPrecondition, "export belongs to a replacement volume")
		}
		if err := c.mutate(ctx, fmt.Sprintf("delete-export %s/%s initiator=%s", id.pool, id.name, e.InitiatorWWN), func(ctx context.Context) error { return c.api.DeleteExport(ctx, id.pool, id.name, e.InitiatorWWN) }); err != nil {
			return nil, err
		}
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *targetdController) validateVolume(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" || len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume and capabilities are required")
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	if strings.HasPrefix(req.GetVolumeId(), "td1_") {
		id, err := c.parseID(req.GetVolumeId())
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(id.name, "_"+policyHash(filesystem(req.GetVolumeCapabilities()[0]))) {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "volume filesystem does not match capabilities"}, nil
		}
		if !matchesVolumeContext(c.volumeContext(), req.GetVolumeContext()) {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "volume context does not match backend"}, nil
		}
		params := req.GetParameters()
		if pool, ok := params["pool"]; ok && pool != id.pool || !validStorageParameters(params) || len(req.GetMutableParameters()) > 0 {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "parameters do not match the volume"}, nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.checkBarrier(ctx); err != nil {
			return nil, err
		}
		vol, err := c.volume(ctx, id)
		if err != nil {
			return nil, err
		}
		if vol == nil {
			return nil, status.Error(codes.NotFound, "volume does not exist")
		}
	} else if err := checkStaticVolume(req.GetVolumeId(), req.GetVolumeContext()); err != nil {
		return nil, err
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeContext: req.GetVolumeContext(), VolumeCapabilities: req.GetVolumeCapabilities(), Parameters: req.GetParameters()}}, nil
}
