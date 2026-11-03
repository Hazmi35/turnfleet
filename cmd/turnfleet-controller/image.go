// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// serviceAccountNamespaceFile holds the namespace a pod runs in, in every
	// pod that has a service account token.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// managerContainerName is the controller's container in config/manager and
	// in the chart.
	managerContainerName = "manager"
)

// discoverRelayHelperImage finds the image the controller runs from, which is
// the image of the init container that decides a relay's bind address: that
// container runs this binary (ADR 23), so the image has to be this one. It
// returns "" when the controller does not run in a pod, or cannot read it, and
// says why. A relay that needs the decision is then refused with an error
// naming the flag and the node annotation that settle it.
func discoverRelayHelperImage(ctx context.Context, reader client.Reader) string {
	namespace, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		setupLog.Info("the relay helper image is not discovered: this process does not run in a pod; set --relay-helper-image",
			"reason", err.Error())
		return ""
	}
	// A pod's hostname is its name, unless the pod sets one of its own.
	name, err := os.Hostname()
	if err != nil {
		setupLog.Info("the relay helper image is not discovered: the pod name is unknown; set --relay-helper-image",
			"reason", err.Error())
		return ""
	}
	image, err := ownImage(ctx, reader, strings.TrimSpace(string(namespace)), name)
	if err != nil {
		setupLog.Info("the relay helper image is not discovered; set --relay-helper-image", "reason", err.Error())
		return ""
	}
	setupLog.Info("relays that decide their bind address on the node run this image", "image", image)
	return image
}

// ownImage reads a pod and returns the image of its manager container, or of
// its only container.
func ownImage(ctx context.Context, reader client.Reader, namespace, name string) (string, error) {
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return "", fmt.Errorf("reading pod %s/%s: %w", namespace, name, err)
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == managerContainerName {
			return pod.Spec.Containers[i].Image, nil
		}
	}
	if len(pod.Spec.Containers) == 1 {
		return pod.Spec.Containers[0].Image, nil
	}
	return "", errors.New("pod " + namespace + "/" + name + " has no container named " + managerContainerName)
}
