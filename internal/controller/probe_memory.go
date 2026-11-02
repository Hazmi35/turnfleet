// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/probe"
)

// probeMemory holds what the controller knows about the probes it ran, per pool
// and relay. It lives in the controller's memory and not in the pool's status:
// the time of every probe changes every probe interval, and status is the place
// for what changes when the relay does (issue #76, ADR 22).
//
// A restarted controller starts with nothing here, so every relay with a ready
// pod is probed in its first pass, the same as a relay that was never probed.
type probeMemory struct {
	mu    sync.Mutex
	pools map[types.NamespacedName]map[string]probeRecord
}

// probeRecord is the last probe the controller ran against a relay.
type probeRecord struct {
	// at is when the probe finished. It is what schedules the next one.
	at time.Time

	// latency is how long the probe took, for turnfleet_probe_duration_seconds.
	latency time.Duration

	// succeeded is whether the probe passed, for turnfleet_probe_success.
	succeeded bool

	// errorCode is the TURN error code the probe saw. It is the one part of
	// the outcome the status does not hold, so a change of it is only noticed
	// while the controller runs.
	errorCode int

	// status is the probe status the controller reports for the relay. Every
	// pass reports it, whether or not it probes, so a status write that failed
	// is made again by the next pass rather than lost.
	status turnfleetv1alpha1.ProbeStatus
}

// statusReason is the API's reason for a probe's reason. It is empty for a
// reason the controller's probe never ends at, which is why the switch names
// every probe reason: the exhaustive linter fails a probe reason added without
// a decision here.
func statusReason(reason probe.Reason) turnfleetv1alpha1.ProbeReason {
	switch reason {
	case probe.ReasonAllocated:
		return turnfleetv1alpha1.ProbeReasonAllocated
	case probe.ReasonListenerUnreachable:
		return turnfleetv1alpha1.ProbeReasonListenerUnreachable
	case probe.ReasonClientFailed:
		return turnfleetv1alpha1.ProbeReasonClientFailed
	case probe.ReasonAllocationRefused:
		return turnfleetv1alpha1.ProbeReasonAllocationRefused
	case probe.ReasonRelayedAddressUnreadable:
		return turnfleetv1alpha1.ProbeReasonRelayedAddressUnreadable
	case probe.ReasonRelayedAddressUnexpected:
		return turnfleetv1alpha1.ProbeReasonRelayedAddressUnexpected
	case probe.ReasonAuthUnavailable:
		return turnfleetv1alpha1.ProbeReasonAuthUnavailable
	case probe.ReasonRoundTripFailed, probe.ReasonURIUnreadable:
		// The controller's probe asks for no peer round trip and parses no
		// URI, so it never ends at either.
		return ""
	}
	return ""
}

func (m *probeMemory) get(pool types.NamespacedName, relayName string) (probeRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.pools[pool][relayName]
	return record, ok
}

func (m *probeMemory) set(pool types.NamespacedName, relayName string, record *probeRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools == nil {
		m.pools = make(map[types.NamespacedName]map[string]probeRecord)
	}
	if m.pools[pool] == nil {
		m.pools[pool] = make(map[string]probeRecord)
	}
	m.pools[pool][relayName] = *record
}

// retain forgets the relays of a pool that are not in keep, so the memory holds
// no relay that no longer exists.
func (m *probeMemory) retain(pool types.NamespacedName, keep map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for relayName := range m.pools[pool] {
		if !keep[relayName] {
			delete(m.pools[pool], relayName)
		}
	}
}

// forget drops every relay of a pool.
func (m *probeMemory) forget(pool types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pools, pool)
}

// nextProbeStatus is a relay's probe status after a probe.
//
// The status records the relay's outcome, not the probe. lastTransitionTime
// moves only when succeeded flips, and reason and message are replaced only
// when the outcome changes, so a relay that keeps answering the same way
// produces the same status and writeStatus skips the write. The message is not
// compared, because it carries the error text, and that text can differ
// between two probes that failed the same way. previousErrorCode is the TURN
// error code of the last probe this controller ran, or nil when it has run
// none since it started.
func nextProbeStatus(current turnfleetv1alpha1.ProbeStatus, previousErrorCode *int, result *probe.Result, now time.Time) turnfleetv1alpha1.ProbeStatus {
	reason := statusReason(result.Reason)
	if current.Succeeded == nil || *current.Succeeded != result.Succeeded || current.LastTransitionTime == nil {
		return turnfleetv1alpha1.ProbeStatus{
			// The API server stores whole seconds. A sub-second time here would
			// differ from the stored one on every later pass and be written
			// again each time.
			LastTransitionTime: new(metav1.NewTime(now.Truncate(time.Second))),
			Succeeded:          new(result.Succeeded),
			Reason:             reason,
			Message:            new(result.Message),
		}
	}
	sameCode := previousErrorCode == nil || *previousErrorCode == result.ErrorCode
	if current.Reason == reason && sameCode {
		return current
	}
	next := current
	next.Reason = reason
	next.Message = new(result.Message)
	return next
}
