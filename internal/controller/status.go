// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/metrics"
)

// Condition types reported on a TURNPool. Ready and ProbeSucceeded are
// normal-true; CapacityPressure, AddressConflict, PodAddressUnusable and
// Degraded are abnormal-true.
const (
	// ConditionReady is true when the desired relays exist, are ready and are
	// not draining.
	ConditionReady = "Ready"

	// ConditionProbeSucceeded is true when every relay that has a pod passed its
	// last TURN Allocate probe.
	ConditionProbeSucceeded = "ProbeSucceeded"

	// ConditionCapacityPressure is true when a relay is near or at its port
	// limit, or a probe was refused with 508 Insufficient Capacity.
	ConditionCapacityPressure = "CapacityPressure"

	// ConditionAddressConflict is true when this pool holds a relay identity
	// that another pool holds too, on the same address and with overlapping
	// ports. Disjoint ranges on a shared address are legal.
	ConditionAddressConflict = "AddressConflict"

	// ConditionPodAddressUnusable is true when a relay node declares a PodIP
	// address the pool cannot advertise at all — unspecified, loopback,
	// link-local or multicast. The address is dropped from the plan, and the
	// condition names the node and the reason. It is the reconcile-time half
	// of a rule admission also checks, because a node annotation can change
	// after the pool was admitted.
	ConditionPodAddressUnusable = "PodAddressUnusable"

	// ConditionReachableFromVantage is true when the latest result published by
	// a vantage runner says every listener URI the pool publishes answered a
	// TURN Allocate where the runner ran, false when one of them did not, and
	// unknown while no current result has been published.
	//
	// It is computed from the published result alone: an in-cluster probe
	// passing cannot clear a failing observation, and a runner is expected to
	// run outside the cluster's network, where hairpin NAT and a firewall in
	// front of a relay node are visible (docs/adr/0007-probe-vantage-point.md,
	// docs/adr/0015-vantage-result-delivery.md).
	ConditionReachableFromVantage = "ReachableFromVantage"

	// ConditionDegraded is true when the pool is not what it should be: relays
	// that could not be placed, a missing pod, an unusable auth Secret, a
	// relay identity that overlaps another pool's, or a vantage point that
	// cannot reach the relays.
	ConditionDegraded = "Degraded"
)

// writeStatus computes and persists the pool's status.
func (r *TURNPoolReconciler) writeStatus(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, inputs *statusInputs) error {
	status := buildStatus(pool, inputs)
	if pool.Status != nil && equality.Semantic.DeepEqual(pool.Status, status) {
		return nil
	}

	name := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &turnfleetv1alpha1.TURNPool{}
		if err := r.Get(ctx, name, latest); err != nil {
			return err
		}
		latest.Status = status
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return fmt.Errorf("updating the status of pool %s: %w", pool.Name, err)
	}
	return nil
}

// buildStatus turns the reconcile's observations into the API's status.
func buildStatus(pool *turnfleetv1alpha1.TURNPool, inputs *statusInputs) *turnfleetv1alpha1.TURNPoolStatus {
	status := &turnfleetv1alpha1.TURNPoolStatus{
		ObservedGeneration: new(pool.Generation),
		Selector:           new(Selector(pool)),
	}

	if inputs == nil {
		inputs = &statusInputs{}
	}
	if inputs.secretMissing != "" {
		// Nothing about the relays can be trusted while the pool cannot
		// authenticate clients, so the status says exactly that.
		setCondition(status, ConditionReady, metav1.ConditionFalse, "AuthSecretMissing", inputs.secretMissing)
		setCondition(status, ConditionProbeSucceeded, metav1.ConditionFalse, "AuthSecretMissing", inputs.secretMissing)
		setCondition(status, ConditionCapacityPressure, metav1.ConditionFalse, "NoRelays", "no relay can be measured without the auth secret")
		setCondition(status, ConditionDegraded, metav1.ConditionTrue, "AuthSecretMissing", inputs.secretMissing)
		// A published result is still readable without the secret, and its
		// failures are already covered by the Degraded reason above.
		_ = writeVantageCondition(status, inputs)
		return status
	}

	counted := countRelays(status, inputs, pool)
	writeConditions(status, pool, counted, inputs)
	return status
}

// relayCounts is what the conditions are computed from.
type relayCounts struct {
	readyRelays   int32
	probeFailures int32
	underPressure int32
	portsInUse    int32
	portsTotal    int32
	degraded      []string
}

// countRelays fills the per-relay status entries and sums the pool's capacity.
func countRelays(status *turnfleetv1alpha1.TURNPoolStatus, inputs *statusInputs, pool *turnfleetv1alpha1.TURNPool) relayCounts {
	states := slices.Clone(inputs.states)
	slices.SortFunc(states, func(a, b *relayState) int {
		return strings.Compare(a.relay.Name, b.relay.Name)
	})

	var counts relayCounts
	counts.degraded = make([]string, 0, 2)
	uris := make([]string, 0, len(states))

	for _, state := range states {
		status.Relays = append(status.Relays, relayStatusFor(state, pool))
		counts.portsInUse += state.portsInUse
		counts.portsTotal += state.relay.PortRange.Ports()

		if state.ready() {
			counts.readyRelays++
			uris = append(uris, state.turnURIs(pool)...)
		}
		if state.planned && !state.draining && state.lastProbe.Succeeded != nil && !*state.lastProbe.Succeeded {
			counts.probeFailures++
		}
		if state.planned && (state.capacityPressure() || state.probeObservedCapacity()) {
			counts.underPressure++
		}
		if state.planned && !state.draining && state.pod == nil {
			counts.degraded = append(counts.degraded, fmt.Sprintf("relay %s has no pod yet", state.relay.Name))
		}
	}
	if inputs.truncated && inputs.plan != nil {
		counts.degraded = append(counts.degraded, inputs.plan.Notes...)
	}

	slices.Sort(uris)
	status.ReadyRelays = new(counts.readyRelays)
	status.PortsInUse = new(counts.portsInUse)
	status.PortsTotal = new(counts.portsTotal)
	status.TurnURIs = uris
	return counts
}

// writeConditions sets the condition types from the counted state, the relay
// identities other pools hold, and the published vantage result.
func writeConditions(status *turnfleetv1alpha1.TURNPoolStatus, pool *turnfleetv1alpha1.TURNPool, counts relayCounts, inputs *statusInputs) {
	desired := pool.Spec.Replicas

	if counts.readyRelays >= desired && len(counts.degraded) == 0 {
		setCondition(status, ConditionReady, metav1.ConditionTrue, "RelaysReady",
			fmt.Sprintf("%d of %d relays are ready", counts.readyRelays, desired))
	} else {
		message := fmt.Sprintf("%d of %d relays are ready", counts.readyRelays, desired)
		if len(counts.degraded) > 0 {
			message = message + ": " + strings.Join(firstN(counts.degraded), "; ")
		}
		setCondition(status, ConditionReady, metav1.ConditionFalse, "RelaysNotReady", message)
	}

	switch {
	case counts.probeFailures > 0:
		setCondition(status, ConditionProbeSucceeded, metav1.ConditionFalse, "ProbeFailed",
			fmt.Sprintf("%d relay(s) failed the TURN Allocate probe", counts.probeFailures))
	case counts.readyRelays == 0:
		setCondition(status, ConditionProbeSucceeded, metav1.ConditionFalse, "NoRelays",
			"no ready relay has been probed yet")
	default:
		setCondition(status, ConditionProbeSucceeded, metav1.ConditionTrue, "ProbeSucceeded",
			fmt.Sprintf("all %d ready relay(s) answered a TURN Allocate with the expected relayed address", counts.readyRelays))
	}

	if counts.underPressure > 0 {
		setCondition(status, ConditionCapacityPressure, metav1.ConditionTrue, "RelayCapacityExhausted",
			fmt.Sprintf("%d relay(s) are at or near their port limit, or refused an allocation with 508; "+
				"add relay addresses (nodes or secondary addresses) to add capacity", counts.underPressure))
	} else {
		setCondition(status, ConditionCapacityPressure, metav1.ConditionFalse, "CapacityAvailable",
			fmt.Sprintf("%d of %d ports in use across %d relay(s)", counts.portsInUse, counts.portsTotal, len(status.Relays)))
	}

	// A conflict with another pool is reported on its own condition and on
	// Degraded: it is a fact about the pool's identity, and it degrades the
	// pool, because a relay that shares its ports with another process serves
	// less than it reports.
	conflictMessage := describeConflicts(inputs.conflicts)
	if len(inputs.conflicts) > 0 {
		setCondition(status, ConditionAddressConflict, metav1.ConditionTrue, "RelayIdentityOverlap", conflictMessage)
	} else {
		setCondition(status, ConditionAddressConflict, metav1.ConditionFalse, "NoOverlap",
			"no other pool holds a relay on this pool's addresses with overlapping ports")
	}

	unreachable := writeVantageCondition(status, inputs)

	// A PodIP address a node declares but the pool cannot advertise is reported
	// on its own condition and on Degraded: the relay behind it is dropped from
	// the plan, so the pool serves less than it asks for until the declaration
	// or the CNI changes.
	podAddressMessage := strings.Join(firstN(inputs.podAddressProblems), "; ")
	if len(inputs.podAddressProblems) > 0 {
		setCondition(status, ConditionPodAddressUnusable, metav1.ConditionTrue, "UnusableRelayAddress", podAddressMessage)
	} else {
		setCondition(status, ConditionPodAddressUnusable, metav1.ConditionFalse, "RelayAddressUsable",
			"every PodIP relay address the pool's nodes declare is individually routable")
	}

	switch {
	case len(inputs.conflicts) > 0:
		setCondition(status, ConditionDegraded, metav1.ConditionTrue, "RelayIdentityOverlap", conflictMessage)
	case len(inputs.podAddressProblems) > 0:
		setCondition(status, ConditionDegraded, metav1.ConditionTrue, "UnusableRelayAddress", podAddressMessage)
	default:
		degraded := append(slices.Clone(counts.degraded), unreachable...)
		if len(degraded) > 0 {
			reason := "RelaysUnavailable"
			if len(counts.degraded) == 0 {
				// The vantage point is the only thing wrong, so it names the
				// reason; otherwise the relay-level failure is the one to act on
				// first and the vantage failure stays in the message and in its own
				// condition.
				reason = reasonUnreachableFromVantage
			}
			setCondition(status, ConditionDegraded, metav1.ConditionTrue, reason, strings.Join(firstN(degraded), "; "))
		} else {
			setCondition(status, ConditionDegraded, metav1.ConditionFalse, "AsExpected", "every planned relay exists and is placeable")
		}
	}
}

// relayStatusFor is one relay's status entry.
func relayStatusFor(state *relayState, pool *turnfleetv1alpha1.TURNPool) turnfleetv1alpha1.RelayStatus {
	status := turnfleetv1alpha1.RelayStatus{
		Name: state.relay.Name,
		PortRange: turnfleetv1alpha1.PortRange{
			Start: state.relay.PortRange.Start,
			End:   state.relay.PortRange.End,
		},
		TurnURIs:    state.turnURIs(pool),
		Ready:       new(state.ready()),
		Draining:    new(state.draining),
		Allocations: new(state.allocations),
		PortsInUse:  new(state.portsInUse),
		PortsTotal:  new(state.relay.PortRange.Ports()),
		LastProbe:   &state.lastProbe,
	}
	address := state.relay.Address
	status.RelayAddress = &address
	nodeName := state.relay.NodeName
	status.NodeName = &nodeName
	if state.pod != nil {
		podName := state.pod.Name
		status.PodName = &podName
	}
	return status
}

// reconcileURIsConfigMap mirrors the ready relays' URIs into the ConfigMap the
// credential issuer reads. It is the only discovery mechanism: there is no
// Service in the data path, so this list is what keeps a client from being sent
// to a relay that is saturated or draining.
func (r *TURNPoolReconciler) reconcileURIsConfigMap(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, states []*relayState) error {
	uris := make([]string, 0, len(states))
	for _, state := range states {
		uris = append(uris, state.turnURIs(pool)...)
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      URIsConfigMapName(pool),
			Namespace: pool.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if configMap.Labels == nil {
			configMap.Labels = map[string]string{}
		}
		maps.Copy(configMap.Labels, Labels(pool))
		if configMap.Annotations == nil {
			configMap.Annotations = map[string]string{}
		}
		configMap.Annotations["turnfleet.hzmi.dev/observed-generation"] = fmt.Sprintf("%d", pool.Generation)
		configMap.Data = map[string]string{
			"uris": strings.Join(uris, "\n"),
		}
		return controllerutil.SetControllerReference(pool, configMap, r.Scheme)
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reconciling configmap %s/%s: %w", pool.Namespace, URIsConfigMapName(pool), err)
	}
	return nil
}

// reconcileDeletion drains every relay before the pool's finalizer is removed,
// so a deleted pool stops accepting new allocations before its relays stop
// serving the ones they hold.
func (r *TURNPoolReconciler) reconcileDeletion(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pool, FinalizerName) {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)
	pods, err := r.relayPods(ctx, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing relay pods during deletion: %w", err)
	}

	// The published result is still read while the pool drains: relays that
	// cannot be reached from the vantage point are worth saying so about until
	// the pool is gone.
	vantageResult, err := r.observeVantage(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(pods) == 0 {
		metrics.ForgetPool(pool.Namespace, pool.Name)
		if err := r.deleteVantageResult(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		original := pool.DeepCopy()
		controllerutil.RemoveFinalizer(pool, FinalizerName)
		if err := r.Patch(ctx, pool, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing the drain finalizer: %w", err)
		}
		log.V(1).Info("pool drained, finalizer removed", "pool", pool.Name, "namespace", pool.Namespace)
		return ctrl.Result{}, nil
	}

	// The URIs go first: whoever reads the ConfigMap must stop handing out
	// relays for a pool that is going away.
	if err := r.clearURIsConfigMap(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}

	states := make([]*relayState, 0, len(pods))
	for i := range pods {
		state, err := orphanState(&pods[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		states = append(states, state)
	}

	draining, err := r.reconcilePods(ctx, pool, states)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A draining pool still holds its ports until its relays are gone, so it
	// still takes part in the cluster-wide identity check.
	conflicts, err := r.addressConflicts(ctx, pool, states)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.writeStatus(ctx, pool, &statusInputs{
		states:    states,
		vantage:   vantageResult,
		now:       r.now(),
		conflicts: conflicts,
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("draining a deleted pool",
		"pool", pool.Name, "namespace", pool.Namespace, "relays", len(states), "draining", draining)
	return ctrl.Result{RequeueAfter: drainPollInterval}, nil
}

// clearURIsConfigMap empties the pool's URI list without deleting the object,
// so the issuer sees an empty list rather than a missing one.
func (r *TURNPoolReconciler) clearURIsConfigMap(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) error {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: URIsConfigMapName(pool)}, configMap)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading configmap %s/%s: %w", pool.Namespace, URIsConfigMapName(pool), err)
	}
	if configMap.Data["uris"] == "" {
		return nil
	}
	patch := client.MergeFrom(configMap.DeepCopy())
	configMap.Data = map[string]string{"uris": ""}
	if err := r.Patch(ctx, configMap, patch); err != nil {
		return fmt.Errorf("clearing configmap %s/%s: %w", pool.Namespace, URIsConfigMapName(pool), err)
	}
	return nil
}

// setCondition writes one condition, preserving the transition time of an
// unchanged one.
func setCondition(status *turnfleetv1alpha1.TURNPoolStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration(status),
	})
}

func observedGeneration(status *turnfleetv1alpha1.TURNPoolStatus) int64 {
	if status.ObservedGeneration == nil {
		return 0
	}
	return *status.ObservedGeneration
}

// firstNShown caps how many of a pool's problems a condition message names, so
// a message stays readable when many relays, peers or listener URIs are
// involved.
const firstNShown = 3

func firstN(values []string) []string {
	if len(values) <= firstNShown {
		return values
	}
	return values[:firstNShown]
}
