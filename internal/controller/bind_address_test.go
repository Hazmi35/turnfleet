// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
)

// externalAddressRulesPath is where the CRD keeps the validation rules of an
// ExternalIPPool entry, below the version's schema.
var externalAddressRulesPath = []string{
	"openAPIV3Schema", "properties", "spec", "properties", "addressing", "properties",
	"externalIPPool", "properties", "addresses", "items",
}

// withoutExternalAddressRules removes the CRD's validation rules of an
// ExternalIPPool entry until the test ends. A cluster that never had them, or
// an object stored before they existed, reaches the controller with a value the
// API server would reject now, and the controller has to cope with it: the
// webhook is optional, and these rules are the only other check.
func withoutExternalAddressRules(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	key := client.ObjectKey{Name: "turnpools.turnfleet.hzmi.dev"}
	if err := k8sClient.Get(ctx, key, crd); err != nil {
		t.Fatalf("reading the TURNPool CRD: %v", err)
	}

	// edit rewrites the rules of the entry schema in the stored version, and
	// returns what it replaced.
	schemaPath := append([]string{"schema"}, externalAddressRulesPath...)
	edit := func(crd *unstructured.Unstructured, rules []any, remove bool) []any {
		versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
		if err != nil || !found {
			t.Fatalf("the CRD has no versions: %v", err)
		}
		var previous []any
		for i := range versions {
			version := versions[i].(map[string]any)
			entry, found, err := unstructured.NestedMap(version, schemaPath...)
			if err != nil || !found {
				t.Fatalf("the CRD has no ExternalIPPool entry schema: %v", err)
			}
			previous, _ = entry["x-kubernetes-validations"].([]any)
			if remove {
				delete(entry, "x-kubernetes-validations")
			} else {
				entry["x-kubernetes-validations"] = rules
			}
			if err := unstructured.SetNestedMap(version, entry, schemaPath...); err != nil {
				t.Fatalf("writing the entry schema: %v", err)
			}
		}
		if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
			t.Fatalf("writing the CRD versions: %v", err)
		}
		return previous
	}

	rules := edit(crd, nil, true)
	if len(rules) == 0 {
		t.Fatal("the CRD carries no validation rules for an ExternalIPPool entry: this test would prove nothing")
	}
	if err := k8sClient.Update(ctx, crd); err != nil {
		t.Fatalf("removing the entry rules from the CRD: %v", err)
	}
	t.Cleanup(func() {
		restored := &unstructured.Unstructured{}
		restored.SetGroupVersionKind(crd.GroupVersionKind())
		if err := k8sClient.Get(context.Background(), key, restored); err != nil {
			t.Errorf("reading the TURNPool CRD to restore its rules: %v", err)
			return
		}
		edit(restored, rules, false)
		if err := k8sClient.Update(context.Background(), restored); err != nil {
			t.Errorf("restoring the entry rules of the CRD: %v", err)
		}
	})
}

// TestReconcileExternalIPPoolSkipsAnUnusableBindAddress covers the controller
// when nothing rejected a bad bindAddress: the entry is skipped with a note in
// the Ready condition, and the pool's other relays are created and become
// ready. Before the planner skipped the entry, the renderer's error stopped the
// whole reconcile, so a typo in one entry halted every relay of the pool.
func TestReconcileExternalIPPoolSkipsAnUnusableBindAddress(t *testing.T) {
	const name = "tpool-ext-badbind"

	withoutExternalAddressRules(t)

	f := newFixture(t)
	f.createSecret()
	f.createNode("tf-badbind-1", "10.42.0.161")
	f.createNode("tf-badbind-2", "10.42.0.162")

	pool := natPool(name, 51000)
	pool.Spec.Replicas = 2
	pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
		Mode: turnfleetv1alpha1.AddressingModeExternalIPPool,
		ExternalIPPool: &turnfleetv1alpha1.ExternalIPPoolSpec{
			Addresses: []turnfleetv1alpha1.ExternalAddress{
				{NodeName: "tf-badbind-1", Address: "203.0.113.71", BindAddress: "not-an-ip"},
				{NodeName: "tf-badbind-2", Address: "203.0.113.72", BindAddress: "10.42.0.162"},
			},
		},
	}
	// The rules leave the API server as the CRD update propagates, so the pool
	// is created once the server accepts the value.
	err := wait.PollUntilContextTimeout(f.ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		candidate := pool.DeepCopy()
		candidate.Namespace = f.namespace
		if candidate.Spec.Dataplane.Image == "" {
			candidate.Spec.Dataplane.Image = "coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3"
		}
		candidate.Spec.Auth.Realm = "turnfleet.test"
		candidate.Spec.Auth.SecretRef = pool.Spec.Auth.SecretRef
		return k8sClient.Create(ctx, candidate) == nil, nil
	})
	if err != nil {
		t.Fatalf("the API server still enforces the entry rules: %v", err)
	}

	f.reconcile(name)
	f.reconcile(name)

	pods := f.pods(name)
	if len(pods) != 1 {
		t.Fatalf("got %d relay pods, want 1: the entry with the unusable bindAddress is skipped and the other places its relay", len(pods))
	}
	if got := pods[0].Annotations[RelayBindAddressAnnotation]; got != "10.42.0.162" {
		t.Errorf("the relay that was placed binds %q, want 10.42.0.162", got)
	}

	f.markPodsReady(name)
	f.reconcileUntil(name, "the placed relay ready", func() bool { return readyRelays(t, f, name) == 1 })

	status := f.pool(name).Status
	if len(status.Relays) != 1 || status.Relays[0].RelayAddress == nil || *status.Relays[0].RelayAddress != "203.0.113.72" {
		t.Fatalf("status relays = %+v, want one relay on 203.0.113.72", status.Relays)
	}
	ready := ""
	for _, condition := range status.Conditions {
		if condition.Type == ConditionReady {
			ready = condition.Message
			if condition.Status != "False" {
				t.Errorf("Ready is %s, want False: only one of two relays can be placed", condition.Status)
			}
		}
	}
	requireContains(t, "Ready message", ready, "203.0.113.71", "not-an-ip", "is not an IP address")
}
