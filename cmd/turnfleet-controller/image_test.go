// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"strings"
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

// TestOwnImageIsPinnedToTheDigestTheKubeletReports covers the image the init
// container runs: the digest of the controller's own image when the kubelet
// reports one, so a reused tag does not give the relay another binary.
func TestOwnImageIsPinnedToTheDigestTheKubeletReports(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		image   string
		imageID string
		want    string
	}{
		{name: "a runtime that reports the repository digest", image: "ghcr.io/hazmi35/turnfleet-controller:v1", imageID: "ghcr.io/hazmi35/turnfleet-controller@" + digest, want: "ghcr.io/hazmi35/turnfleet-controller:v1@" + digest},
		{name: "a docker-pullable image ID", image: "turnfleet-controller:e2e", imageID: "docker-pullable://turnfleet-controller@" + digest, want: "turnfleet-controller:e2e@" + digest},
		{name: "an image that has a digest already", image: "example.com/c@" + digest, imageID: "example.com/c@sha256:" + strings.Repeat("cd", 32), want: "example.com/c@" + digest},
		{name: "a runtime that reports only the content ID", image: "turnfleet-controller:e2e", imageID: digest, want: "turnfleet-controller:e2e"},
		{name: "no image ID yet", image: "turnfleet-controller:e2e", imageID: "", want: "turnfleet-controller:e2e"},
		{name: "an image ID that is not a digest", image: "turnfleet-controller:e2e", imageID: "repo@notadigest", want: "turnfleet-controller:e2e"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "turnfleet-system", Name: "manager-abc"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: tc.image}}},
				Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", ImageID: tc.imageID}}},
			}
			got, err := ownImage(t.Context(), fake.NewClientBuilder().WithObjects(pod).Build(), "turnfleet-system", "manager-abc")
			if err != nil {
				t.Fatalf("ownImage() failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("ownImage() = %q, want %q", got, tc.want)
			}
		})
	}
}
