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
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type fakeBarrierMaps struct {
	typedcorev1.ConfigMapInterface
	config    *corev1.ConfigMap
	getErr    error
	updateErr error
}

func (f *fakeBarrierMaps) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.config.DeepCopy(), nil
}

func (f *fakeBarrierMaps) Update(_ context.Context, config *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.config = config.DeepCopy()
	return config.DeepCopy(), nil
}

func newFakeBarrier() (*kubernetesBarrier, *fakeBarrierMaps) {
	maps := &fakeBarrierMaps{config: &corev1.ConfigMap{}}
	return &kubernetesBarrier{configMaps: func() (typedcorev1.ConfigMapInterface, error) { return maps, nil }}, maps
}

func TestMutationBarrier(t *testing.T) {
	ctx := context.Background()
	barrier, maps := newFakeBarrier()
	if err := barrier.Check(ctx); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := barrier.Mutate(ctx, "create-volume", func(context.Context) error {
		called = true
		if maps.config.Data[targetdBarrierKey] == "" {
			t.Error("operation ran before durable marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || barrier.Check(ctx) != nil {
		t.Fatal("successful mutation did not clear marker")
	}

	backendErr := errors.New("ambiguous response")
	if err := barrier.Mutate(ctx, "create-export", func(context.Context) error { return backendErr }); !errors.Is(err, backendErr) {
		t.Fatalf("unexpected backend error: %v", err)
	}
	if status.Code(barrier.Check(ctx)) != codes.FailedPrecondition {
		t.Fatal("ambiguous mutation did not block inventory and retry")
	}
	called = false
	if err := barrier.Mutate(ctx, "delete-volume", func(context.Context) error { called = true; return nil }); status.Code(err) != codes.FailedPrecondition || called {
		t.Fatalf("pending mutation allowed another operation: %v", err)
	}
	// A new controller process must observe the same durable marker.
	restarted := &kubernetesBarrier{configMaps: func() (typedcorev1.ConfigMapInterface, error) { return maps, nil }}
	if status.Code(restarted.Check(ctx)) != codes.FailedPrecondition {
		t.Fatal("restart lost pending mutation")
	}
}

func TestMutationBarrierUnavailable(t *testing.T) {
	ctx := context.Background()
	barrier, maps := newFakeBarrier()
	maps.getErr = errors.New("API unavailable")
	called := false
	if err := barrier.Check(ctx); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected API error: %v", err)
	}
	if err := barrier.Mutate(ctx, "create-volume", func(context.Context) error { called = true; return nil }); status.Code(err) != codes.Unavailable || called {
		t.Fatalf("operation ran without durable marker: %v", err)
	}
	maps.getErr = nil
	maps.updateErr = errors.New("API unavailable")
	if err := barrier.Mutate(ctx, "create-volume", func(context.Context) error { called = true; return nil }); status.Code(err) != codes.Unavailable || called {
		t.Fatalf("operation ran after marker write failed: %v", err)
	}
	maps.updateErr = nil
	if err := barrier.Mutate(ctx, "create-volume", func(context.Context) error {
		maps.updateErr = errors.New("API unavailable")
		return nil
	}); status.Code(err) != codes.Unavailable {
		t.Fatalf("marker clear failure: %v", err)
	}
	maps.updateErr = nil
	if status.Code(barrier.Check(ctx)) != codes.FailedPrecondition {
		t.Fatal("uncleared marker did not block further work")
	}
}

func TestMutationBarrierDoesNotClearDifferentMarker(t *testing.T) {
	barrier, maps := newFakeBarrier()
	err := barrier.Mutate(context.Background(), "create-volume", func(context.Context) error {
		maps.config.Data[targetdBarrierKey] = "other-controller"
		return nil
	})
	if status.Code(err) != codes.Unavailable || maps.config.Data[targetdBarrierKey] != "other-controller" {
		t.Fatalf("changed marker was cleared: %v", err)
	}
}
