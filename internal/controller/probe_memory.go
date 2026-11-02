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

	outcome probeOutcome

	// status is the probe status the controller reports for the relay. Every
	// pass reports it, whether or not it probes, so a status write that failed
	// is made again by the next pass rather than lost.
	status turnfleetv1alpha1.ProbeStatus
}

// probeOutcome is what decides whether a probe changed anything worth writing
// to the status: whether it passed, the step it ended at and the TURN error
// code. The message is left out on purpose, because it carries the error text,
// and that text can differ between two probes that failed the same way.
type probeOutcome struct {
	succeeded bool
	reason    string
	errorCode int
}

func outcomeOf(result *probe.Result) probeOutcome {
	return probeOutcome{succeeded: result.Succeeded, reason: result.Reason, errorCode: result.ErrorCode}
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
// moves only when succeeded flips, and the message is replaced only when the
// outcome does, so a relay that keeps answering the same way produces the same
// status and writeStatus skips the write. previous is the outcome of the last
// probe this controller ran, or nil when it has run none since it started; the
// message is then refreshed once, because there is nothing to compare it with.
func nextProbeStatus(current turnfleetv1alpha1.ProbeStatus, previous *probeOutcome, result *probe.Result, now time.Time) turnfleetv1alpha1.ProbeStatus {
	if current.Succeeded == nil || *current.Succeeded != result.Succeeded || current.LastTransitionTime == nil {
		return turnfleetv1alpha1.ProbeStatus{
			// The API server stores whole seconds. A sub-second time here would
			// differ from the stored one on every later pass and be written
			// again each time.
			LastTransitionTime: new(metav1.NewTime(now.Truncate(time.Second))),
			Succeeded:          new(result.Succeeded),
			Message:            new(result.Message),
		}
	}
	if previous != nil && *previous == outcomeOf(result) {
		return current
	}
	next := current
	next.Message = new(result.Message)
	return next
}
