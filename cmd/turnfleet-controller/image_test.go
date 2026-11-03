// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestOwnImage covers how the controller finds the image its relays' init
// containers run: the manager container of its own pod.
func TestOwnImage(t *testing.T) {
	t.Parallel()

	pod := func(containers ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "turnfleet-system", Name: "turnfleet-controller-manager-abc"},
			Spec:       corev1.PodSpec{Containers: containers},
		}
	}
	manager := corev1.Container{Name: "manager", Image: "ghcr.io/hazmi35/turnfleet-controller:v1.2.3"}
	sidecar := corev1.Container{Name: "kube-rbac-proxy", Image: "example.com/proxy:v1"}

	tests := []struct {
		name    string
		pod     *corev1.Pod
		want    string
		wantErr bool
	}{
		{name: "the manager container", pod: pod(sidecar, manager), want: manager.Image},
		{name: "the only container, whatever its name", pod: pod(corev1.Container{Name: "controller", Image: "turnfleet:dev"}), want: "turnfleet:dev"},
		{name: "several containers and none is the manager", pod: pod(sidecar, corev1.Container{Name: "other", Image: "x:1"}), wantErr: true},
		{name: "the pod does not exist", pod: nil, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			builder := fake.NewClientBuilder()
			if tc.pod != nil {
				builder = builder.WithObjects(tc.pod)
			}
			got, err := ownImage(t.Context(), builder.Build(), "turnfleet-system", "turnfleet-controller-manager-abc")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ownImage() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ownImage() failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("ownImage() = %q, want %q", got, tc.want)
			}
		})
	}
}
