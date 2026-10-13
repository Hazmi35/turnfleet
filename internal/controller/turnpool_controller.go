// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package controller reconciles TURNPools into relays.
//
// The reconcile loop is level-based and idempotent: it observes the relay
// nodes, the pool's Secret and its existing relay pods, computes the desired
// relay set with the planner, renders each relay's coturn configuration, and
// converges. Relays are identified by (address, port range), so a restart, a
// leader change or an unrelated reconcile never moves one; a relay is replaced
// in place (drain, then recreate) when its configuration changes.
package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/metrics"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// Constants that bound how a pool converges.
const (
	// secretRetryInterval is how often a pool without its auth Secret is
	// rechecked: the Secret is usually created a moment after the pool.
	secretRetryInterval = 10 * time.Second

	// drainPollInterval is how often an in-flight drain is re-evaluated. It is
	// short because the drain is what makes a rollout take as long as it does.
	drainPollInterval = time.Second

	// capacityPressurePercent is the percentage of a relay's ports that must be
	// in use before CapacityPressure is raised, before a probe observes a 508.
	capacityPressurePercent = 90

	// scrapeTimeout bounds reading one relay's metrics endpoint.
	scrapeTimeout = 2 * time.Second
)

// TURNPoolReconciler reconciles a TURNPool object.
type TURNPoolReconciler struct {
	client.Client

	// Scheme is the manager's scheme.
	Scheme *runtime.Scheme

	// Recorder publishes Kubernetes events.
	Recorder recorder.EventRecorder

	// Now is the clock. Injectable so tests can drive the drain timeout and the
	// probe interval without sleeping.
	Now func() time.Time

	// Prober runs the TURN Allocate check against a relay. The default is
	// probe.Allocate.
	Prober func(ctx context.Context, cfg *probe.Config) probe.Result

	// Scraper reads a relay's coturn metrics. The default scrapes over HTTP.
	Scraper func(ctx context.Context, url string) (metrics.CoturnSample, error)

	// ClusterCIDRs are peer CIDRs the pool's relays must always deny, from the
	// controller's flags: the cluster's service range when it is not RFC 1918.
	ClusterCIDRs []string
}

// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile converges one TURNPool.
func (r *TURNPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool := &turnfleetv1alpha1.TURNPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, pool)
	}
	if !controllerutil.ContainsFinalizer(pool, FinalizerName) {
		// Add the guard and carry on: the rest of the reconcile does not depend
		// on the finalizer already being persisted, and returning here would
		// only delay the first relay by one pass.
		if err := r.addFinalizer(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.V(1).Info("reconciling pool",
		"pool", pool.Name, "namespace", pool.Namespace, "generation", pool.Generation)

	observed, err := r.observeNodes(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing relay nodes: %w", err)
	}
	pods, err := r.relayPods(ctx, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing relay pods: %w", err)
	}

	secret, err := r.sharedSecret(ctx, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading the auth secret: %w", err)
	}
	if secret == "" {
		// Without the shared secret a relay would refuse every credential, so
		// there is nothing sensible to converge: say so and retry.
		message := fmt.Sprintf("secret %s/%s has no key %q: relays cannot authenticate clients",
			pool.Namespace, pool.Spec.Auth.SecretRef.Name, pool.Spec.Auth.SecretRef.Key)
		r.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, "AuthSecretMissing", "Validate", "%s", message)
		if err := r.writeStatus(ctx, pool, &statusInputs{secretMissing: message}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: secretRetryInterval}, nil
	}

	plan := relay.ComputePlan(&relay.PlanInput{PoolName: pool.Name, Spec: pool.Spec, Nodes: observed.relays})
	relay.SortRelays(plan.Relays)

	states, err := r.renderRelays(pool, &plan, observed.podCIDRs, secret, pods)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRelayConfigMap(ctx, pool, states); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the relay configuration: %w", err)
	}

	draining, err := r.reconcilePods(ctx, pool, states)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling relay pods: %w", err)
	}

	r.measureRelays(ctx, pool, states)

	// Relay identities are unique cluster-wide, so the pass that computes
	// them also compares them with the identities the other pools hold.
	conflicts, err := r.addressConflicts(ctx, pool, states)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.writeStatus(ctx, pool, &statusInputs{
		plan:      &plan,
		states:    states,
		truncated: plan.Unplaced > 0,
		conflicts: conflicts,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileURIsConfigMap(ctx, pool, states); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the turn URIs: %w", err)
	}

	return ctrl.Result{RequeueAfter: r.requeueAfter(draining, pool)}, nil
}

// requeueAfter picks the next reconcile: a second while a drain is in flight,
// otherwise one probe interval.
func (r *TURNPoolReconciler) requeueAfter(draining bool, pool *turnfleetv1alpha1.TURNPool) time.Duration {
	if draining {
		return drainPollInterval
	}
	return probeInterval(pool)
}

// probeInterval is how often relays are probed.
func probeInterval(pool *turnfleetv1alpha1.TURNPool) time.Duration {
	if pool.Spec.Probe == nil {
		return relay.DefaultProbeInterval
	}
	return relay.DurationOrDefault(pool.Spec.Probe.Interval, relay.DefaultProbeInterval)
}

// probeTimeout bounds a single probe.
func probeTimeout(pool *turnfleetv1alpha1.TURNPool) time.Duration {
	if pool.Spec.Probe == nil {
		return relay.DefaultProbeTimeout
	}
	return relay.DurationOrDefault(pool.Spec.Probe.Timeout, relay.DefaultProbeTimeout)
}

// drainTimeout bounds how long a draining relay keeps its allocations.
func drainTimeout(pool *turnfleetv1alpha1.TURNPool) time.Duration {
	return relay.DurationOrDefault(pool.Spec.DrainTimeout, relay.DefaultDrainTimeout)
}

// nodeObservation is the relay-relevant view of the cluster's nodes.
type nodeObservation struct {
	// relays are the relay nodes, with the addresses they would advertise.
	relays []relay.Node

	// podCIDRs are the pod networks of those nodes, which relays must never be
	// able to reach.
	podCIDRs []string
}

// observeNodes lists the nodes that may host relays.
func (r *TURNPoolReconciler) observeNodes(ctx context.Context) (*nodeObservation, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels{RelayNodeLabel: labelValueTrue}); err != nil {
		return nil, err
	}

	observed := &nodeObservation{relays: make([]relay.Node, 0, len(nodes.Items))}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		local := nodeAddress(node)
		advertised := local
		if override := node.Annotations[RelayAddressAnnotation]; override != "" {
			advertised = override
		}
		observed.relays = append(observed.relays, relay.Node{
			Name:        node.Name,
			Address:     advertised,
			BindAddress: local,
		})
		if node.Spec.PodCIDR != "" {
			observed.podCIDRs = append(observed.podCIDRs, node.Spec.PodCIDR)
		}
		for _, cidr := range node.Spec.PodCIDRs {
			if cidr != "" {
				observed.podCIDRs = append(observed.podCIDRs, cidr)
			}
		}
	}
	slices.SortFunc(observed.relays, func(a, b relay.Node) int { return strings.Compare(a.Name, b.Name) })
	return observed, nil
}

// nodeAddress is the address a relay on this node binds: the node's ExternalIP,
// or its InternalIP when there is none.
func nodeAddress(node *corev1.Node) string {
	var internal string
	for _, addr := range node.Status.Addresses {
		switch addr.Type {
		case corev1.NodeExternalIP:
			if addr.Address != "" {
				return addr.Address
			}
		case corev1.NodeInternalIP:
			if internal == "" {
				internal = addr.Address
			}
		case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
			// Names are not addresses: a relay cannot bind or advertise them.
		}
	}
	return internal
}

func (r *TURNPoolReconciler) addFinalizer(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) error {
	original := pool.DeepCopy()
	controllerutil.AddFinalizer(pool, FinalizerName)
	if err := r.Patch(ctx, pool, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("adding the drain finalizer: %w", err)
	}
	return nil
}

func (r *TURNPoolReconciler) relayPods(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	selector := labels.SelectorFromSet(SelectorLabels(pool))
	if err := r.List(ctx, pods, client.InNamespace(pool.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// sharedSecret reads the shared HMAC secret the credential issuer also uses.
func (r *TURNPoolReconciler) sharedSecret(ctx context.Context, pool *turnfleetv1alpha1.TURNPool) (string, error) {
	ref := pool.Spec.Auth.SecretRef
	if ref == nil || ref.Name == "" || ref.Key == "" {
		return "", nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: ref.Name}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading secret %s/%s: %w", pool.Namespace, ref.Name, err)
	}
	return string(secret.Data[ref.Key]), nil
}

func (r *TURNPoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager.
func (r *TURNPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder(ControllerName)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&turnfleetv1alpha1.TURNPool{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		// A relay node losing its label, or changing its address, changes the
		// plan, so nodes are watched as well.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.poolsForNode),
			builder.WithPredicates(nodeChanged())).
		// Relay identities must be unique across pools, and the pool that has
		// to react to another pool's identity is not the one that changed it,
		// so a change in what a pool holds wakes the pools sharing its
		// addresses.
		Watches(&turnfleetv1alpha1.TURNPool{}, handler.EnqueueRequestsFromMapFunc(r.poolsForHeldAddresses),
			builder.WithPredicates(heldIdentitiesChanged())).
		Named(ControllerName).
		Complete(r)
}

// listPools lists every TURNPool in the cluster. Relay identities are unique
// across the cluster, so a caller that needs the other pools' identities cannot
// scope the list to one namespace or label.
func (r *TURNPoolReconciler) listPools(ctx context.Context) ([]turnfleetv1alpha1.TURNPool, error) {
	pools := &turnfleetv1alpha1.TURNPoolList{}
	if err := r.List(ctx, pools); err != nil {
		return nil, err
	}
	return pools.Items, nil
}

// poolsForNode maps a relay node to the pools that could place a relay on it.
func (r *TURNPoolReconciler) poolsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	if _, isRelayNode := node.Labels[RelayNodeLabel]; !isRelayNode {
		return nil
	}
	pools, err := r.listPools(ctx)
	if err != nil {
		logf.FromContext(ctx).Error(err, "listing pools for a relay node change", "node", node.Name)
		return nil
	}
	requests := make([]ctrl.Request, 0, len(pools))
	for i := range pools {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: pools[i].Namespace,
			Name:      pools[i].Name,
		}})
	}
	return requests
}

// nodeChanged triggers on the two node properties that move relays: the relay
// label and the address a relay would advertise.
func nodeChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, oldOK := e.ObjectOld.(*corev1.Node)
			newNode, newOK := e.ObjectNew.(*corev1.Node)
			if !oldOK || !newOK {
				return true
			}
			return oldNode.Labels[RelayNodeLabel] != newNode.Labels[RelayNodeLabel] ||
				nodeAddress(oldNode) != nodeAddress(newNode) ||
				oldNode.Annotations[RelayAddressAnnotation] != newNode.Annotations[RelayAddressAnnotation]
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// lastProbeFromStatus finds the previous probe result for a relay, so a
// reconcile does not re-probe every relay on every pass.
func lastProbeFromStatus(prev []turnfleetv1alpha1.RelayStatus, name string) (probeStatus turnfleetv1alpha1.ProbeStatus, allocations int32) {
	for i := range prev {
		if prev[i].Name != name {
			continue
		}
		if prev[i].Allocations != nil {
			allocations = *prev[i].Allocations
		}
		if prev[i].LastProbe != nil {
			probeStatus = *prev[i].LastProbe
		}
		return probeStatus, allocations
	}
	return probeStatus, allocations
}

// podReady reports whether the kubelet considers a relay pod ready.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// findPod returns the pod with the given name, or nil.
func findPod(pods []corev1.Pod, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}

// parseTime parses an RFC 3339 annotation, returning the zero time when it is
// missing or unreadable.
func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// statusInputs is what writeStatus needs beyond the relay states.
type statusInputs struct {
	plan      *relay.Plan
	states    []*relayState
	truncated bool

	// secretMissing is set when the pool's auth Secret is unusable, which makes
	// the whole pool Degraded regardless of what the relays are doing.
	secretMissing string

	// conflicts are the relay identities this pool holds that another pool
	// holds too, with overlapping ports.
	conflicts []addressConflict
}
