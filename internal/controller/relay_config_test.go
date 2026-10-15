// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// TestRelayConfigurationIsHeldByASecret is the delivery property the rendered
// configuration is published under. It embeds the shared HMAC secret, so it is
// a Secret and never a ConfigMap, and the relay reads it from there: anything
// allowed to read ConfigMaps in the namespace would otherwise be able to mint
// credentials for the pool's relays.
func TestRelayConfigurationIsHeldByASecret(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-config-node", "203.0.113.61")
	f.createPool(hostNetworkPoolOn("tpool-config", 49300))

	f.reconcile("tpool-config")

	pods := f.pods("tpool-config")
	if len(pods) != 1 {
		t.Fatalf("got %d relay pods, want 1", len(pods))
	}

	name := RelayConfigSecretName(f.pool("tpool-config"))
	rendered := f.relayConfig("tpool-config")
	if !strings.Contains(rendered, "static-auth-secret="+fixtureSecret) {
		t.Errorf("the rendered configuration does not carry the pool's shared secret:\n%s", rendered)
	}

	configMap := &corev1.ConfigMap{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, configMap); !apierrors.IsNotFound(err) {
		t.Errorf("the rendered configuration is readable as the ConfigMap %s/%s (err=%v)", f.namespace, name, err)
	}

	var source *corev1.VolumeSource
	for i := range pods[0].Spec.Volumes {
		if pods[0].Spec.Volumes[i].Name == configVolumeName {
			source = &pods[0].Spec.Volumes[i].VolumeSource
		}
	}
	switch {
	case source == nil:
		t.Fatalf("relay pod %s has no %s volume", pods[0].Name, configVolumeName)
	case source.ConfigMap != nil || source.Secret == nil:
		t.Errorf("relay pod %s mounts its configuration from %+v, want a Secret", pods[0].Name, *source)
	case source.Secret.SecretName != name:
		t.Errorf("relay pod %s mounts secret %q, want %q", pods[0].Name, source.Secret.SecretName, name)
	}
}

// TestSupersededRelayConfigMapIsDeleted covers the upgrade path from the
// releases before ADR 20 kept the rendered configuration in a ConfigMap. That
// object holds the shared secret in the clear, so the pass that writes the
// Secret takes it away instead of leaving it next to it.
func TestSupersededRelayConfigMapIsDeleted(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-superseded-node", "203.0.113.62")
	f.createPool(hostNetworkPoolOn("tpool-superseded", 49340))

	pool := f.pool("tpool-superseded")
	name := RelayConfigSecretName(pool)
	superseded := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Data:       map[string]string{"tpool-superseded-relay-abcde": "static-auth-secret=" + fixtureSecret},
	}
	if err := controllerutil.SetControllerReference(pool, superseded, testScheme); err != nil {
		t.Fatalf("setting the owner of the superseded ConfigMap: %v", err)
	}
	if err := k8sClient.Create(f.ctx, superseded); err != nil {
		t.Fatalf("creating the superseded ConfigMap: %v", err)
	}

	f.reconcile("tpool-superseded")

	configMap := &corev1.ConfigMap{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, configMap); !apierrors.IsNotFound(err) {
		t.Errorf("the superseded ConfigMap %s/%s survived the pass (err=%v); it holds the shared secret in the clear", f.namespace, name, err)
	}
}

// TestUnrelatedConfigMapIsNotDeleted pins the guard on that cleanup: the
// operator deletes the object it wrote, not one that happens to carry the name.
func TestUnrelatedConfigMapIsNotDeleted(t *testing.T) {
	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-unrelated-node", "203.0.113.63")
	f.createPool(hostNetworkPoolOn("tpool-unrelated", 49380))

	name := RelayConfigSecretName(f.pool("tpool-unrelated"))
	unrelated := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Data:       map[string]string{"keep": "me"},
	}
	if err := k8sClient.Create(f.ctx, unrelated); err != nil {
		t.Fatalf("creating the unrelated ConfigMap: %v", err)
	}

	f.reconcile("tpool-unrelated")

	configMap := &corev1.ConfigMap{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, configMap); err != nil {
		t.Errorf("the pass deleted a ConfigMap the pool does not own: %v", err)
	}
}
