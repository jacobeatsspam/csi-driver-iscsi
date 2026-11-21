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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/targetd"
)

type TargetdConfig struct {
	BackendID    string   `json:"backendID"`
	Endpoint     string   `json:"endpoint"`
	Pools        []string `json:"pools"`
	Namespace    string   `json:"namespace"`
	TargetIQN    string   `json:"targetIQN"`
	Portals      []string `json:"portals"`
	Username     string   `json:"username"`
	PasswordFile string   `json:"passwordFile"`
	CAFile       string   `json:"caFile"`
}

// NewTargetdControllerDriver creates a controller-only process from a mounted configuration.
func NewTargetdControllerDriver(endpoint, path string) (*driver, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read targetd configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("targetd configuration is too large")
	}
	var cfg TargetdConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, errors.New("invalid targetd configuration")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid targetd configuration")
	}
	if cfg.BackendID == "" || cfg.Namespace == "" || len(cfg.Pools) == 0 || len(cfg.Portals) == 0 || !validInitiator(cfg.TargetIQN) {
		return nil, errors.New("backend ID, namespace, pools, target IQN and portals are required")
	}
	seen := map[string]bool{}
	for _, pool := range cfg.Pools {
		key := poolHash(pool)
		if pool == "" || strings.ContainsAny(pool, " \t\r\n/") || seen[key] {
			return nil, errors.New("invalid or conflicting targetd pool")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for i, portal := range cfg.Portals {
		portal, err = validatePortal(portal)
		if err != nil {
			return nil, err
		}
		addr, err := netip.ParseAddrPort(portal)
		if err != nil || !addr.Addr().Is4() {
			return nil, errors.New("targetd data portals require IPv4 addresses")
		}
		portal = addr.String()
		if seen[portal] {
			return nil, errors.New("duplicate target portal")
		}
		cfg.Portals[i] = portal
		seen[portal] = true
	}
	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return nil, errors.New("cannot read targetd password file")
	}
	var ca []byte
	if cfg.CAFile != "" {
		ca, err = os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, errors.New("cannot read targetd CA file")
		}
	}
	client, err := targetd.New(targetd.Config{Endpoint: cfg.Endpoint, Username: cfg.Username, Password: strings.TrimSuffix(string(password), "\n"), CA: ca})
	if err != nil {
		return nil, err
	}
	barrier, err := newKubernetesBarrier()
	if err != nil {
		return nil, err
	}
	d := &driver{name: driverName, version: version, endpoint: endpoint, targetd: &targetdController{api: client, backendID: cfg.BackendID, namespace: cfg.Namespace, iqn: cfg.TargetIQN, pools: cfg.Pools, portals: cfg.Portals, checkNode: checkKubernetesNode, checkDetach: checkKubernetesDetach, barrier: barrier}}
	d.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER})
	d.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME, csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME})
	return d, nil
}
