// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/metrics"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// relayState is everything one reconcile pass knows about one relay.
type relayState struct {
	// relay is the planned relay, or the relay an orphaned pod serves.
	relay relay.Relay

	// planned reports whether the relay is part of the desired set. A relay
	// that is not planned is on its way out.
	planned bool

	// config is the rendered coturn configuration and hash is its digest.
	config string
	hash   string

	// pod is the relay's pod, or nil when it does not exist yet.
	pod *corev1.Pod

	// draining reports whether the relay is going away.
	draining bool

	// addressRequest is the declaration a PodIP relay pod asks its attachment
	// for, prefix included, so the pod is given the address the relay was
	// planned with rather than whichever address the IPAM picks. It is empty
	// for every other mode and for a PodIP pool without an attachment.
	addressRequest string

	// addressChanged reports whether the scheduled pod holds another address
	// than the one the relay was planned with. It is the PodIP mode's
	// stability check: a mismatch drains the relay (ADR 6).
	addressChanged bool

	// observedAddress is the address the pod actually holds, for the drain
	// event and the log line.
	observedAddress string

	// drainStarted is when the relay was first seen draining.
	drainStarted time.Time

	// allocations and portsInUse are the last known counts from coturn.
	allocations int32
	portsInUse  int32

	// lastProbe is the most recent probe result.
	lastProbe turnfleetv1alpha1.ProbeStatus

	// probeErrorCode is the TURN error code the probe observed in this pass, or
	// 0. 508 (Insufficient Capacity) is the saturation signal.
	probeErrorCode int

	// probeDue reports whether this relay should be probed in this pass.
	probeDue bool
}

// ready reports whether the relay is serving: its pod is ready, it is not
// draining, and its last probe succeeded.
func (s *relayState) ready() bool {
	if s.pod == nil || s.draining || !podReady(s.pod) {
		return false
	}
	if s.lastProbe.Succeeded == nil {
		// Not probed yet: a relay that has not answered a real Allocate is not
		// announced to clients.
		return false
	}
	return *s.lastProbe.Succeeded
}

// turnURIs are the listener URIs clients may allocate on right now. A relay
// that is not ready, or is draining, has none: the credential issuer reads the
// same list, so a client never wastes a gathering cycle on a relay that cannot
// take it.
func (s *relayState) turnURIs(pool *turnfleetv1alpha1.TURNPool) []string {
	if !s.ready() {
		return nil
	}
	return relay.ListenerURIs(&s.relay, pool.Spec.Listeners)
}

// capacityPressure reports whether the relay's ports are (nearly) exhausted.
func (s *relayState) capacityPressure() bool {
	total := s.relay.PortRange.Ports()
	if total <= 0 {
		return false
	}
	return int64(s.portsInUse)*100 >= int64(total)*capacityPressurePercent
}

// probeObservedCapacity reports whether this pass's probe was refused with 508,
// which is unambiguous proof that the relay is full: coturn only answers 508
// when no port in the range is left.
func (s *relayState) probeObservedCapacity() bool {
	return s.probeErrorCode == turnErrorInsufficientCapacity
}

// renderRelays joins the plan with the existing pods and each relay's rendered
// configuration.
func (r *TURNPoolReconciler) renderRelays(
	pool *turnfleetv1alpha1.TURNPool,
	plan *relay.Plan,
	observed *nodeObservation,
	secret string,
	tlsSecretHash string,
	pods []corev1.Pod,
) ([]*relayState, error) {
	denied := relay.MergeCIDRs(policyDeniedCIDRs(pool), r.ClusterCIDRs, observed.podCIDRs)
	requests := podAddressRequests(observed)
	var allowed []string
	if pool.Spec.PeerPolicy != nil {
		allowed = pool.Spec.PeerPolicy.AllowedCIDRs
	}

	states := make([]*relayState, 0, len(plan.Relays)+len(pods))
	seen := make(map[string]bool, len(plan.Relays))

	for i := range plan.Relays {
		planned := plan.Relays[i]
		config, err := relay.Render(&relay.RenderInput{
			Relay:         planned,
			PoolNamespace: pool.Namespace,
			PoolName:      pool.Name,
			Spec:          pool.Spec,
			DeniedCIDRs:   denied,
			AllowedCIDRs:  allowed,
			AuthSecret:    secret,
		})
		if err != nil {
			return nil, fmt.Errorf("rendering relay %s: %w", planned.Name, err)
		}

		state := &relayState{
			relay:          planned,
			planned:        true,
			config:         config,
			addressRequest: requests[planned.NodeName+"|"+planned.Address],
		}
		hash, err := relayHash(pool, state, tlsSecretHash)
		if err != nil {
			return nil, err
		}
		state.hash = hash
		state.lastProbe, state.allocations = lastProbeFromStatus(previousRelays(pool), planned.Name)
		state.portsInUse = state.allocations

		if pod := findPod(pods, planned.Name); pod != nil {
			state.pod = pod
			state.drainStarted = parseTime(pod.Annotations[DrainStartedAtAnnotation])
			state.draining = !state.drainStarted.IsZero()
			state.observedAddress, _ = relayAddressOnPod(pool, pod)
			state.addressChanged = relayAddressChanged(pool, &state.relay, pod)
		}
		state.probeDue = r.probeDue(pool, state)

		states = append(states, state)
		seen[planned.Name] = true
	}

	// Relays that exist but are no longer planned are draining: they keep their
	// pod until their allocations are gone or drainTimeout expires.
	for i := range pods {
		pod := &pods[i]
		if seen[pod.Name] {
			continue
		}
		state, err := orphanState(pod)
		if err != nil {
			return nil, err
		}
		state.lastProbe, state.allocations = lastProbeFromStatus(previousRelays(pool), pod.Name)
		state.portsInUse = state.allocations
		states = append(states, state)
	}

	return states, nil
}

// policyDeniedCIDRs is the pool's own peer deny list, or the operator's default
// when the pool does not carry one.
//
// The defaulting webhook applies the same default, but it is not the only
// install path: config/default runs the controller without admission webhooks,
// and a spec that reaches the controller from there has been defaulted by
// nobody. A relay whose only denied peers are the ones this controller knows
// about is an SSRF pivot into the cluster, so the operator applies the default
// itself rather than trusting that someone else did — and applies the same one,
// chosen from the pool's family, because a relay only reaches peers in its own
// family and the list of the other one would deny nothing (ADR 13).
func policyDeniedCIDRs(pool *turnfleetv1alpha1.TURNPool) []string {
	if pool.Spec.PeerPolicy != nil && len(pool.Spec.PeerPolicy.DeniedCIDRs) > 0 {
		return pool.Spec.PeerPolicy.DeniedCIDRs
	}
	return relay.DefaultDeniedCIDRsFor(pool.Spec.Addressing.Family)
}

// previousRelays is the pool's last observed relay list, which is empty when the
// status has never been written.
func previousRelays(pool *turnfleetv1alpha1.TURNPool) []turnfleetv1alpha1.RelayStatus {
	if pool.Status == nil {
		return nil
	}
	return pool.Status.Relays
}

// orphanState describes a relay pod the plan no longer contains.
func orphanState(pod *corev1.Pod) (*relayState, error) {
	identity := pod.Annotations[RelayIdentityAnnotation]
	address, portRange, err := relay.ParseIdentity(identity)
	if err != nil {
		return nil, fmt.Errorf("relay pod %s has an unreadable identity %q: %w", pod.Name, identity, err)
	}
	state := &relayState{
		relay: relay.Relay{
			Name:        pod.Name,
			Identity:    identity,
			NodeName:    pod.Spec.NodeName,
			Address:     address,
			BindAddress: pod.Annotations[RelayBindAddressAnnotation],
			PortRange:   portRange,
		},
		pod: pod,
	}
	// The drain clock starts when the relay is first marked draining, not when
	// the pod was created: reading a missing annotation as "draining since the
	// beginning of time" would delete a relay that still holds allocations.
	state.drainStarted = parseTime(pod.Annotations[DrainStartedAtAnnotation])
	state.draining = !state.drainStarted.IsZero()
	return state, nil
}

// probeDue reports whether a relay should be probed in this pass: relays whose
// probe result is missing or older than the interval, and relays that are
// draining.
func (r *TURNPoolReconciler) probeDue(pool *turnfleetv1alpha1.TURNPool, state *relayState) bool {
	if state.pod == nil || !podReady(state.pod) {
		return false
	}
	if state.lastProbe.Time == nil {
		return true
	}
	return !r.now().Before(state.lastProbe.Time.Add(probeInterval(pool)))
}

// measureRelays probes the relays that are due and reads every reachable
// relay's coturn metrics, then republishes the pool's metrics.
func (r *TURNPoolReconciler) measureRelays(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, states []*relayState) {
	for _, state := range states {
		if state.pod == nil {
			continue
		}
		if state.probeDue {
			result := r.probeOnce(ctx, pool, state)
			state.lastProbe = probeStatusFrom(&result)
			state.probeErrorCode = result.ErrorCode
			metrics.SetProbe(pool.Namespace, pool.Name, state.relay.Name, result.Succeeded,
				time.Duration(result.LatencyMillis)*time.Millisecond)
			if !result.Succeeded && result.ErrorCode != 0 {
				metrics.RecordAllocationFailure(pool.Namespace, pool.Name, state.relay.Name, result.ErrorCode)
			}
		}
		state.allocations = r.scrapeAllocations(ctx, state)
		state.portsInUse = state.allocations
	}

	// Drop the series of relays that no longer exist, then republish: a stale
	// series is a lie about capacity.
	metrics.ForgetPool(pool.Namespace, pool.Name)
	for _, state := range states {
		metrics.SetRelay(pool.Namespace, pool.Name, state.relay.Name,
			state.allocations, state.portsInUse, state.relay.PortRange.Ports())
	}
}

// probeOnce runs the TURN Allocate check for one relay against the relay's own
// address, which is what a client on the same network would do.
func (r *TURNPoolReconciler) probeOnce(ctx context.Context, pool *turnfleetv1alpha1.TURNPool, state *relayState) probe.Result {
	secret, err := r.sharedSecret(ctx, pool)
	if err != nil || secret == "" {
		logf.FromContext(ctx).V(1).Info("skipping probe without a usable auth secret", "relay", state.relay.Name)
		return probe.Result{
			Succeeded: false,
			Message:   "the pool's auth secret could not be read, so the probe cannot authenticate",
		}
	}

	username, password := probe.Credentials(secret, probe.DefaultUser, probe.DefaultCredentialTTL, r.now())
	portRange := state.relay.PortRange
	cfg := &probe.Config{
		Endpoint: probe.Endpoint{
			Host:      state.relay.Address,
			Port:      listenerPort(pool.Spec.Listeners, state.relay.ListenerPortOffset),
			Transport: probe.TransportUDP,
		},
		Username:        username,
		Password:        password,
		Timeout:         probeTimeout(pool),
		ExpectAddress:   state.relay.Address,
		ExpectPortRange: &portRange,
	}

	prober := r.Prober
	if prober == nil {
		prober = probe.Allocate
	}
	return prober(ctx, cfg)
}

// scrapeAllocations reads a relay's current allocation count from coturn.
//
// A scrape failure keeps the last known count: the relay's probe decides
// readiness, and inventing capacity pressure out of a timeout would be worse
// than reporting a stale number.
func (r *TURNPoolReconciler) scrapeAllocations(ctx context.Context, state *relayState) int32 {
	podIP := state.pod.Status.PodIP
	if podIP == "" {
		return state.allocations
	}
	url, err := metrics.MetricsURL(podIP, state.relay.MetricsPort(), relay.DefaultPrometheusPath)
	if err != nil {
		return state.allocations
	}

	scraper := r.Scraper
	if scraper == nil {
		scraper = func(ctx context.Context, url string) (metrics.CoturnSample, error) {
			client := newScrapeClient()
			return metrics.ScrapeCoturn(ctx, client, url)
		}
	}
	sample, err := scraper(ctx, url)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("could not scrape relay metrics",
			"relay", state.relay.Name, "url", url, "error", err.Error())
		return state.allocations
	}
	return sample.Allocations
}

// turnErrorInsufficientCapacity is the TURN error code coturn answers when every
// port in a relay's range is taken.
const turnErrorInsufficientCapacity = 508

// newScrapeClient is the HTTP client used to read relay metrics. It is short:
// a relay that cannot answer is reported as a scrape error, not waited for.
func newScrapeClient() *http.Client {
	return &http.Client{Timeout: scrapeTimeout}
}

// listenerPort is the port the controller probes: the plain listener when there
// is one, otherwise the TLS listener. Relays that share an address each get
// their own listener port, so the offset matters.
func listenerPort(listeners []turnfleetv1alpha1.ListenerSpec, portOffset int32) int32 {
	var tlsPort int32
	for _, listener := range listeners {
		switch listener.Protocol {
		case turnfleetv1alpha1.ListenerProtocolUDP, turnfleetv1alpha1.ListenerProtocolTCP:
			return listener.Port + portOffset
		case turnfleetv1alpha1.ListenerProtocolTLS:
			tlsPort = listener.Port + portOffset
		}
	}
	return tlsPort
}

// probeStatusFrom converts a probe result into the API's probe status.
func probeStatusFrom(result *probe.Result) turnfleetv1alpha1.ProbeStatus {
	now := metav1.Now()
	succeeded := result.Succeeded
	latency := result.LatencyMillis
	message := result.Message
	return turnfleetv1alpha1.ProbeStatus{
		Time:          &now,
		Succeeded:     &succeeded,
		LatencyMillis: &latency,
		Message:       &message,
	}
}
