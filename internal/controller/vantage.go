// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/vantage"
)

// VantageResultMaxAge is how old a published result may be before the pool
// stops reporting it as the current one.
//
// The shipped runner (config/vantage) runs every five minutes, so this is three
// missed passes: long enough that one slow or failed pass does not flip the
// condition, short enough that a runner that stopped hours ago cannot leave
// "reachable" standing. A runner on a different schedule makes this constant
// part of what has to change with it.
const VantageResultMaxAge = 15 * time.Minute

// Condition reasons the vantage result produces.
const (
	reasonReachableFromVantage   = "ReachableFromVantage"
	reasonUnreachableFromVantage = "UnreachableFromVantage"
	reasonNoVantageResult        = "NoVantageResult"
	reasonVantageResultStale     = "VantageResultStale"
	reasonVantageResultInvalid   = "VantageResultInvalid"
	reasonNoVantageTargets       = "NoVantageTargets"
)

// vantageObservation is what one reconcile pass read from the pool's vantage
// ConfigMap.
type vantageObservation struct {
	// payload is the published result, when there is a usable one.
	payload *vantage.Payload

	// problem explains why a published result could not be used. An empty
	// problem with a nil payload means nothing has been published.
	problem string
}

// observed reports whether the observation holds a usable result.
func (o vantageObservation) observed() bool {
	return o.payload != nil
}

// observeVantage reads the result a runner published for the pool.
//
// It reads the ConfigMap rather than the pool's spec or status on purpose: the
// published result is the only thing an outside vantage point can write, and
// the controller must not be able to reconstruct it from anything it observes
// in-cluster. A missing ConfigMap is not an error — it means no runner has run.
func (r *TURNPoolReconciler) observeVantage(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) (vantageObservation, error) {
	name := vantage.ConfigMapName(pool.Name)
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, configMap)
	switch {
	case apierrors.IsNotFound(err):
		return vantageObservation{}, nil
	case err != nil:
		return vantageObservation{}, fmt.Errorf("reading configmap %s/%s: %w", pool.Namespace, name, err)
	}

	payload, err := vantage.Parse(configMap, configMap.Data, pool.Name)
	if errors.Is(err, vantage.ErrNoResult) {
		return vantageObservation{}, nil
	}
	if err != nil {
		// An unreadable payload is data, not a failure to retry: nothing about
		// reconciling again makes the object readable, and the condition says
		// exactly what is wrong with it.
		//nolint:nilerr // the parse error is carried in the observation.
		return vantageObservation{problem: err.Error()}, nil
	}
	return vantageObservation{payload: payload}, nil
}

// deleteVantageResult removes a published result.
//
// The runner creates the ConfigMap without an owner reference, because the
// operator does not own the runner, so nothing else would collect it when the
// pool goes.
func (r *TURNPoolReconciler) deleteVantageResult(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) error {
	name := vantage.ConfigMapName(pool.Name)
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: name}}
	if err := r.Delete(ctx, configMap); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting configmap %s/%s: %w", pool.Namespace, name, err)
	}
	return nil
}

// writeVantageCondition sets the pool's vantage condition and returns what the
// Degraded condition should say about it.
//
// The condition is computed from the published payload alone. Nothing the
// controller observes in-cluster takes part in it, which is what keeps a
// passing in-cluster probe from clearing a failing observation: a pool whose
// relays answer inside the cluster and nowhere else reports exactly that.
func writeVantageCondition(status *turnfleetv1alpha1.TURNPoolStatus, inputs *statusInputs) []string {
	observation := inputs.vantage
	conditionStatus, reason, message := vantageCondition(observation, inputs.now)
	setCondition(status, ConditionReachableFromVantage, conditionStatus, reason, message)

	if conditionStatus != metav1.ConditionFalse {
		return nil
	}
	failures := make([]string, 0, len(observation.payload.Failures()))
	for _, result := range observation.payload.Failures() {
		failures = append(failures, fmt.Sprintf("the vantage point could not reach %s: %s", result.URI, result.Message))
	}
	return failures
}

// vantageCondition turns a published result into a condition.
func vantageCondition(observation vantageObservation, now time.Time) (conditionStatus metav1.ConditionStatus, reason, message string) {
	switch {
	case observation.problem != "":
		return metav1.ConditionUnknown, reasonVantageResultInvalid,
			fmt.Sprintf("the published vantage result cannot be read: %s", observation.problem)
	case !observation.observed():
		return metav1.ConditionUnknown, reasonNoVantageResult,
			"no vantage runner has published a result for this pool; see config/vantage"
	}

	payload := observation.payload
	if now.IsZero() {
		// A caller that did not set a clock cannot judge freshness, and
		// treating a result as fresh on no evidence is the one thing this
		// condition must not do.
		now = time.Now()
	}
	if age := now.Sub(payload.ObservedAt); age > VantageResultMaxAge {
		return metav1.ConditionUnknown, reasonVantageResultStale,
			fmt.Sprintf("the latest vantage result was observed at %s, %s ago, which is older than %s",
				payload.ObservedAt.UTC().Format(time.RFC3339), age.Round(time.Second), VantageResultMaxAge)
	}
	if len(payload.Results) == 0 {
		return metav1.ConditionUnknown, reasonNoVantageTargets,
			fmt.Sprintf("the vantage result observed at %s holds no listener URI: the pool published none when the runner ran",
				payload.ObservedAt.UTC().Format(time.RFC3339))
	}

	if failed := payload.Failures(); len(failed) > 0 {
		uris := make([]string, 0, len(failed))
		for _, result := range failed {
			uris = append(uris, fmt.Sprintf("%s (%s)", result.URI, result.Message))
		}
		return metav1.ConditionFalse, reasonUnreachableFromVantage,
			fmt.Sprintf("%d of %d listener URIs did not answer from the vantage point at %s: %s",
				len(failed), len(payload.Results), payload.ObservedAt.UTC().Format(time.RFC3339),
				strings.Join(firstN(uris), "; "))
	}

	return metav1.ConditionTrue, reasonReachableFromVantage,
		fmt.Sprintf("all %d listener URI(s) answered a TURN Allocate from the vantage point at %s",
			len(payload.Results), payload.ObservedAt.UTC().Format(time.RFC3339))
}

// poolsForVantageResult maps a published result to the pool it is about, so a
// runner's write triggers a reconcile as soon as it lands.
func (r *TURNPoolReconciler) poolsForVantageResult(_ context.Context, obj client.Object) []ctrl.Request {
	poolName, ok := vantage.PoolForConfigMap(obj)
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      poolName,
	}}}
}
