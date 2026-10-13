//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/controller"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
	"github.com/Hazmi35/turnfleet/internal/vantage"
)

// Environment this file reads on top of the suite's. The Makefile sets it.
const (
	envProbeImage = "PROBE_IMG"

	// defaultProbeImage is the image config/vantage/runner.yaml names, and what
	// `make load-images` loads into the kind cluster.
	defaultProbeImage = "ghcr.io/hazmi35/turnfleet-probe:dev"

	// vantageManifest is the runner manifest a user applies, relative to the
	// module root.
	vantageManifest = "config/vantage/runner.yaml"

	// vantageJob is the one-shot Job the suite creates from the shipped
	// CronJob, the documented `kubectl create job` shape.
	vantageJob = "vantage-runner-manual"

	// vantagePool is both the pool this test creates and the pool the shipped
	// manifest targets, so the manifest is applied as a user would apply it.
	vantagePool = "demo"
)

// probeImage is the probe image the runner Job must run.
var probeImage = env(envProbeImage, defaultProbeImage)

// TestVantageRunnerInCluster runs the shipped runner manifest in the cluster
// and checks that the controller reports what it published.
//
// The runner runs on a pod network, so this proves the delivery path and the
// condition, not that a peer outside the cluster can reach the relay: for that
// the same binary has to run outside the cluster's network. See
// docs/adr/0015-vantage-result-delivery.md.
func TestVantageRunnerInCluster(t *testing.T) {
	ctx := context.Background()

	createPool(t, &turnfleetv1alpha1.TURNPool{
		ObjectMeta: metav1.ObjectMeta{Name: vantagePool},
		Spec: turnfleetv1alpha1.TURNPoolSpec{
			Replicas:       1,
			Addressing:     turnfleetv1alpha1.AddressingSpec{Mode: turnfleetv1alpha1.AddressingModeHostNetwork},
			RelayPortRange: turnfleetv1alpha1.PortRange{Start: 51700, End: 51710},
			Listeners: []turnfleetv1alpha1.ListenerSpec{
				{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 34787},
			},
			PeerPolicy: &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()},
		},
	})

	// waitForRelays returns only once the controller's own in-cluster probe has
	// succeeded, which is the result the vantage point must not be able to hide
	// behind.
	status := waitForRelays(ctx, t, vantagePool, 1)
	relayStatus := status.Status.Relays[0]

	applyVantageManifest(t)
	runVantageRunner(t)

	waitFor(ctx, t, "the vantage result to be reported as reachable", 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			return condition(ctx, t, vantagePool, controller.ConditionReachableFromVantage) == metav1.ConditionTrue, nil
		})

	// The condition must come from the published result, not from the pool:
	// check the payload the runner wrote and the values it holds.
	payload := publishedPayload(ctx, t)
	if len(payload.Results) != 1 {
		t.Fatalf("the runner published %d results, want one per listener URI: %+v", len(payload.Results), payload.Results)
	}
	result := payload.Results[0]
	if !result.Succeeded {
		t.Fatalf("the runner's probe failed inside the cluster: %s", result.Message)
	}
	if result.RelayedAddress != *relayStatus.RelayAddress {
		t.Errorf("the runner probed %s and the relay advertised %s", result.URI, result.RelayedAddress)
	}

	// Now the rule the whole delivery path exists for: a failing observation
	// stays in the status even while the in-cluster probe keeps passing.
	suspendVantageRunner(t, true)
	publishFailingResult(ctx, t, relayStatus.TurnURIs[0])

	waitFor(ctx, t, "the failing vantage result to be reported", 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			return condition(ctx, t, vantagePool, controller.ConditionReachableFromVantage) == metav1.ConditionFalse, nil
		})

	current := pool(ctx, t, vantagePool)
	if got := conditionAt(current, controller.ConditionProbeSucceeded); got != metav1.ConditionTrue {
		t.Errorf("the in-cluster probe reports %s while the vantage point reports unreachable, want True: "+
			"the in-cluster result is what must not mask it", got)
	}
	degraded := conditionAt(current, controller.ConditionDegraded)
	if degraded != metav1.ConditionTrue {
		t.Errorf("Degraded is %s while the vantage point cannot reach the pool, want True", degraded)
	}
}

// applyVantageManifest applies config/vantage/runner.yaml the way a user does:
// one file, the namespace substituted for the suite's.
//
// The pool name is not substituted: the manifest targets the pool the sample
// manifest and this test both call `demo`.
func applyVantageManifest(t *testing.T) {
	t.Helper()

	path := filepath.Join(repoRoot(t), vantageManifest)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", vantageManifest, err)
	}

	manifest := string(raw)
	// The manifest ships for `namespace: default`; the suite runs in its own.
	manifest = strings.ReplaceAll(manifest, "namespace: default", "namespace: "+testNamespace)
	// The Makefile's image, which `make load-images` put on the node.
	manifest = strings.ReplaceAll(manifest, defaultProbeImage, probeImage)
	if !strings.Contains(manifest, "namespace: "+testNamespace) {
		t.Fatalf("%s no longer carries 'namespace: default' to retarget", vantageManifest)
	}

	applied := filepath.Join(t.TempDir(), "runner.yaml")
	//nolint:gosec // the path is this test's own temporary directory.
	if err := os.WriteFile(applied, []byte(manifest), 0o600); err != nil {
		t.Fatalf("writing the runner manifest: %v", err)
	}

	kubectl(context.Background(), t, "apply", "-f", applied)
	t.Cleanup(func() { kubectl(context.Background(), t, "delete", "-f", applied, "--ignore-not-found") })
}

// runVantageRunner creates the one-shot Job every test here runs from the
// shipped CronJob and waits for it to finish.
func runVantageRunner(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	kubectl(ctx, t, "create", "job", "--from", "cronjob/vantage-runner", vantageJob, "-n", testNamespace)
	t.Cleanup(func() { deleteVantageJob(ctx, t) })

	waitFor(ctx, t, "the vantage runner Job to succeed", 2*time.Minute, func(ctx context.Context) (bool, error) {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: vantageJob}, job); err != nil {
			return false, err
		}
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("the runner Job failed: %s: %s", condition.Reason, condition.Message)
			}
		}
		return job.Status.Succeeded > 0, nil
	})
}

// deleteVantageJob removes the one-shot Job, so the next run can create it.
func deleteVantageJob(ctx context.Context, t *testing.T) {
	t.Helper()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: vantageJob}}
	propagation := metav1.DeletePropagationBackground
	err := k8sClient.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("deleting job %s: %v", vantageJob, err)
	}
}

// suspendVantageRunner stops the CronJob from publishing over the result the
// test publishes by hand.
func suspendVantageRunner(t *testing.T, suspended bool) {
	t.Helper()

	kubectl(context.Background(), t, "patch", "cronjob", "vantage-runner", "-n", testNamespace,
		"--type", "merge", "-p", fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspended))
}

// publishedPayload reads the result the runner published.
func publishedPayload(ctx context.Context, t *testing.T) *vantage.Payload {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	name := vantage.ConfigMapName(vantagePool)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, configMap); err != nil {
		t.Fatalf("reading the published result %s: %v", name, err)
	}
	payload, err := vantage.Parse(configMap, configMap.Data, vantagePool)
	if err != nil {
		t.Fatalf("decoding the published result %s: %v", name, err)
	}
	return payload
}

// publishFailingResult publishes a result that says the pool is unreachable, as
// a runner outside the cluster would when a firewall or a NAT rule stops it.
func publishFailingResult(ctx context.Context, t *testing.T, uri string) {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	name := vantage.ConfigMapName(vantagePool)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, configMap); err != nil {
		t.Fatalf("reading the published result %s: %v", name, err)
	}

	payload := &vantage.Payload{
		Pool:       vantagePool,
		ObservedAt: time.Now(),
		Results: []probe.Result{{
			URI:     uri,
			Message: "cannot reach the relay listener: simulated failure from outside the cluster",
		}},
	}
	data, err := payload.Data()
	if err != nil {
		t.Fatalf("encoding the failing result: %v", err)
	}
	patch := client.MergeFrom(configMap.DeepCopy())
	configMap.Data = data
	if err := k8sClient.Patch(ctx, configMap, patch); err != nil {
		t.Fatalf("publishing a failing result: %v", err)
	}
}

// condition reads a pool's condition status, failing on an unreadable pool.
func condition(ctx context.Context, t *testing.T, poolName, conditionType string) metav1.ConditionStatus {
	t.Helper()
	return conditionAt(pool(ctx, t, poolName), conditionType)
}

// conditionAt reads one condition out of a pool's status.
func conditionAt(pool *turnfleetv1alpha1.TURNPool, conditionType string) metav1.ConditionStatus {
	if pool.Status == nil {
		return metav1.ConditionUnknown
	}
	for _, condition := range pool.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}
