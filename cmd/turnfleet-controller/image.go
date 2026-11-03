// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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

// ownImageResolver returns the function the reconciler calls when a relay needs
// the image of its init container: the image the controller runs from, because
// that container runs this binary (ADR 23). It reads the controller's own pod
// each time it is called, and the reconciler keeps the first answer. An error is
// returned to the reconciler, which retries, so one failed read does not
// leave the process without the image for good. Outside a pod there is nothing
// to read, and the error says to set --relay-helper-image.
func ownImageResolver(reader client.Reader) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		namespace, err := os.ReadFile(serviceAccountNamespaceFile)
		if err != nil {
			return "", fmt.Errorf("this process does not run in a pod, so it cannot read its own image: set --relay-helper-image: %w", err)
		}
		// A pod's hostname is its name, unless the pod sets one of its own.
		name, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("the pod name is unknown: set --relay-helper-image: %w", err)
		}
		image, err := ownImage(ctx, reader, strings.TrimSpace(string(namespace)), name)
		if err != nil {
			return "", fmt.Errorf("reading the controller's own image: set --relay-helper-image to skip it: %w", err)
		}
		setupLog.Info("relays that decide their bind address on the node run this image", "image", image)
		return image, nil
	}
}

// ownImage reads a pod and returns the image of its manager container, or of
// its only container, pinned to the digest the kubelet reports for it when it
// reports one.
func ownImage(ctx context.Context, reader client.Reader, namespace, name string) (string, error) {
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return "", fmt.Errorf("reading pod %s/%s: %w", namespace, name, err)
	}
	container := ""
	image := ""
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == managerContainerName {
			container, image = managerContainerName, pod.Spec.Containers[i].Image
		}
	}
	if container == "" {
		if len(pod.Spec.Containers) != 1 {
			return "", errors.New("pod " + namespace + "/" + name + " has no container named " + managerContainerName)
		}
		container, image = pod.Spec.Containers[0].Name, pod.Spec.Containers[0].Image
	}
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == container {
			return pinImage(image, pod.Status.ContainerStatuses[i].ImageID), nil
		}
	}
	return image, nil
}

// sha256Digest matches the digest of an image reference.
var sha256Digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// pinImage adds the digest the kubelet reports in a container's imageID to the
// image, so a relay's init container runs the bytes the controller runs even
// when the tag is reused. It returns the image unchanged when it already has a
// digest, or when the kubelet reports none that can be pulled: some runtimes
// report only the image's content ID. The tag is then all there is, and a node
// that already holds an image under that tag keeps it (the init container's
// pull policy is IfNotPresent).
func pinImage(image, imageID string) string {
	if strings.Contains(image, "@") {
		return image
	}
	_, digest, found := strings.CutLast(imageID, "@")
	if !found || !sha256Digest.MatchString(digest) {
		return image
	}
	return image + "@" + digest
}
