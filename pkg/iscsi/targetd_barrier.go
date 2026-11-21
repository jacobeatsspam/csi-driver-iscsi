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
	"crypto/rand"
	"encoding/hex"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const targetdBarrierName = "csi-iscsi-targetd-barrier"
const targetdBarrierKey = "pending"

type mutationBarrier interface {
	Check(context.Context) error
	Mutate(context.Context, string, func(context.Context) error) error
}

type kubernetesBarrier struct {
	configMaps func() (typedcorev1.ConfigMapInterface, error)
}

func newKubernetesBarrier() (mutationBarrier, error) {
	return &kubernetesBarrier{configMaps: func() (typedcorev1.ConfigMapInterface, error) {
		namespace := os.Getenv("POD_NAMESPACE")
		if namespace == "" {
			return nil, status.Error(codes.Unavailable, "controller namespace is not configured")
		}
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "cannot access mutation barrier")
		}
		client, err := typedcorev1.NewForConfig(config)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "cannot access mutation barrier")
		}
		return client.ConfigMaps(namespace), nil
	}}, nil
}

func (b *kubernetesBarrier) Check(ctx context.Context) error {
	configMaps, err := b.configMaps()
	if err != nil {
		return err
	}
	config, err := configMaps.Get(ctx, targetdBarrierName, metav1.GetOptions{})
	if err != nil {
		return status.Error(codes.Unavailable, "cannot read mutation barrier")
	}
	if _, pending := config.Data[targetdBarrierKey]; pending {
		return status.Error(codes.FailedPrecondition, "targetd mutation requires manual recovery")
	}
	return nil
}

func (b *kubernetesBarrier) Mutate(ctx context.Context, operation string, fn func(context.Context) error) error {
	configMaps, err := b.configMaps()
	if err != nil {
		return err
	}
	config, err := configMaps.Get(ctx, targetdBarrierName, metav1.GetOptions{})
	if err != nil {
		return status.Error(codes.Unavailable, "cannot read mutation barrier")
	}
	if _, pending := config.Data[targetdBarrierKey]; pending {
		return status.Error(codes.FailedPrecondition, "targetd mutation requires manual recovery")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return status.Error(codes.Unavailable, "cannot create mutation marker")
	}
	marker := operation + ":" + hex.EncodeToString(random[:])
	if config.Data == nil {
		config.Data = make(map[string]string)
	}
	config.Data[targetdBarrierKey] = marker
	if _, err := configMaps.Update(ctx, config, metav1.UpdateOptions{}); err != nil {
		return status.Error(codes.Unavailable, "cannot write mutation barrier")
	}
	if err := fn(ctx); err != nil {
		return err
	}
	config, err = configMaps.Get(ctx, targetdBarrierName, metav1.GetOptions{})
	if err != nil || config.Data[targetdBarrierKey] != marker {
		return status.Error(codes.Unavailable, "cannot confirm mutation barrier")
	}
	delete(config.Data, targetdBarrierKey)
	if _, err := configMaps.Update(ctx, config, metav1.UpdateOptions{}); err != nil {
		return status.Error(codes.Unavailable, "cannot clear mutation barrier")
	}
	return nil
}
