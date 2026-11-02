// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/probe"
)

// probeOutcomeFixture is a pool with one ready, passing relay, and the passes
// that probe it.
type probeOutcomeFixture struct {
	*testFixture

	name     string
	interval time.Duration
}

func newProbeOutcomeFixture(t *testing.T, name, nodeName, address string, portStart int32) *probeOutcomeFixture {
	t.Helper()

	f := newFixture(t)
	f.createSecret()
	f.createNode(nodeName, address)
	f.createPool(hostNetworkPoolOn(name, portStart))
	f.reconcile(name)
	f.reconcile(name)
	f.markPodsReady(name)
	f.reconcileUntil(name, "one ready relay", func() bool {
		return readyRelays(t, f, name) == 1
	})
	f.reconcile(name)
	return &probeOutcomeFixture{testFixture: f, name: name, interval: probeInterval(f.pool(name))}
}

// probePass runs the first pass a probe interval later, which has to probe the
// relay, and returns the mutating requests it issued.
func (f *probeOutcomeFixture) probePass(what string) []string {
	f.t.Helper()

	calls := len(f.prober.calls)
	mutations := f.api.count()
	f.clock.Advance(f.interval)
	f.reconcile(f.name)
	if len(f.prober.calls) != calls+1 {
		f.t.Fatalf("%s: the pass a probe interval later ran %d probe(s), want 1", what, len(f.prober.calls)-calls)
	}
	return f.api.since(mutations)
}

// lastProbe is the relay's probe outcome as the pool reports it.
func (f *probeOutcomeFixture) lastProbe() turnfleetv1alpha1.ProbeStatus {
	f.t.Helper()

	pool := f.pool(f.name)
	if pool.Status == nil || len(pool.Status.Relays) != 1 || pool.Status.Relays[0].LastProbe == nil {
		f.t.Fatalf("the pool reports no probe outcome for its relay: %+v", pool.Status)
	}
	return *pool.Status.Relays[0].LastProbe
}

// expectOutcome fails the test unless the relay's outcome is the one given.
func (f *probeOutcomeFixture) expectOutcome(what string, succeeded bool, message string, since time.Time) {
	f.t.Helper()

	got := f.lastProbe()
	if got.Succeeded == nil || *got.Succeeded != succeeded {
		f.t.Errorf("%s: succeeded = %s, want %t", what, describePointer(got.Succeeded), succeeded)
	}
	if message != "" && (got.Message == nil || *got.Message != message) {
		f.t.Errorf("%s: message = %s, want %q", what, describePointer(got.Message), message)
	}
	if got.LastTransitionTime == nil || !got.LastTransitionTime.Time.Equal(since) {
		f.t.Errorf("%s: lastTransitionTime = %v, want %v", what, got.LastTransitionTime, since)
	}
}

func describePointer[T any](value *T) string {
	if value == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%#v", *value)
}

func failingProbe(reason, message string) *probe.Result {
	return &probe.Result{Succeeded: false, Reason: reason, Message: message}
}

// TestProbeStatusChangesOnlyWithTheOutcome is the regression test for issue
// #76: every probe used to rewrite the pool's status, because the status
// carried the time and the latency of the last probe. The status now carries
// the relay's outcome, so a probe that answers like the one before it writes
// nothing, and the passes that do write are the ones where the outcome moved.
func TestProbeStatusChangesOnlyWithTheOutcome(t *testing.T) {
	f := newProbeOutcomeFixture(t, "tpool-probe-outcome", "tf-probe-outcome", "203.0.113.79", 52800)

	passing := f.lastProbe()
	if passing.Succeeded == nil || !*passing.Succeeded || passing.LastTransitionTime == nil {
		t.Fatalf("the ready relay's probe outcome is %+v, want a pass with its transition time", passing)
	}

	// The same outcome, probe after probe: nothing is written.
	for i := range 3 {
		if issued := f.probePass("passing again"); len(issued) != 0 {
			t.Errorf("probe %d with an unchanged outcome issued %v; it must write nothing", i+1, issued)
		}
	}
	if got := f.lastProbe(); !equality.Semantic.DeepEqual(got, passing) {
		t.Errorf("an unchanged outcome moved the probe status from %+v to %+v", passing, got)
	}

	// The relay starts failing: that is a transition, and it is written. The
	// clock is off the whole second from here on, and the API server stores
	// whole seconds: a transition time kept with its fraction would differ from
	// the stored one and be written again by every later pass.
	f.clock.Advance(500 * time.Millisecond)
	const firstFailure = "cannot reach the relay listener: attempt 1"
	f.prober.result = failingProbe(probe.ReasonListenerUnreachable, firstFailure)
	if issued := f.probePass("first failure"); len(issued) == 0 {
		t.Fatal("the probe that first failed wrote nothing: the status still says the relay passes")
	}
	failedAt := f.clock.Now().Truncate(time.Second)
	f.expectOutcome("after the first failure", false, firstFailure, failedAt)

	// The same failure with other error text, which is what a TURN client error
	// carrying a transaction ID looks like: not a transition, nothing written.
	f.prober.result = failingProbe(probe.ReasonListenerUnreachable, "cannot reach the relay listener: attempt 2")
	if issued := f.probePass("same failure"); len(issued) != 0 {
		t.Errorf("a failure at the same step with other error text issued %v; it must write nothing", issued)
	}
	f.expectOutcome("after the same failure again", false, firstFailure, failedAt)

	// A failure at another step replaces the message, and the relay has been
	// failing since the first failure all the same.
	const otherFailure = "the relay refused the allocation"
	f.prober.result = failingProbe(probe.ReasonAllocationRefused, otherFailure)
	if issued := f.probePass("another failure"); len(issued) == 0 {
		t.Error("a failure at another step wrote nothing: the message still names the old one")
	}
	f.expectOutcome("after a failure at another step", false, otherFailure, failedAt)

	// The relay recovers: a transition again.
	f.prober.result = nil
	if issued := f.probePass("recovery"); len(issued) == 0 {
		t.Fatal("the probe that passed again wrote nothing: the status still says the relay fails")
	}
	f.expectOutcome("after the relay recovered", true, "", f.clock.Now().Truncate(time.Second))
}

// TestRestartedControllerProbesWithoutWriting covers the boundary the memory
// adds: a restarted controller remembers no probe, so it probes every relay in
// its first pass. The outcome it finds is the one the status holds, so that
// probe writes nothing either.
func TestRestartedControllerProbesWithoutWriting(t *testing.T) {
	f := newProbeOutcomeFixture(t, "tpool-probe-restart", "tf-probe-restart", "203.0.113.89", 52900)
	before := f.lastProbe()

	restarted := &TURNPoolReconciler{
		Client:   f.api,
		Scheme:   testScheme,
		Recorder: events.NewFakeRecorder(100),
		Now:      f.clock.Now,
		Prober:   f.prober.probe,
		Scraper:  f.scraper.scrape,
	}
	calls := len(f.prober.calls)
	mutations := f.api.count()
	if _, err := restarted.Reconcile(f.ctx, ctrlRequest(f.namespace, f.name)); err != nil {
		t.Fatalf("the restarted controller's first pass failed: %v", err)
	}
	if len(f.prober.calls) != calls+1 {
		t.Errorf("the restarted controller's first pass ran %d probe(s), want 1: it has no probe schedule yet", len(f.prober.calls)-calls)
	}
	if issued := f.api.since(mutations); len(issued) != 0 {
		t.Errorf("the restarted controller's first probe found the outcome the status holds and issued %v; it must write nothing", issued)
	}
	if got := f.lastProbe(); !equality.Semantic.DeepEqual(got, before) {
		t.Errorf("the restart moved the probe status from %+v to %+v", before, got)
	}
}

// statusWriteFailure fails every status write, the way an API server that is
// briefly unavailable does.
type statusWriteFailure struct {
	client.Client
}

func (c statusWriteFailure) Status() client.SubResourceWriter {
	return failingStatusWriter{SubResourceWriter: c.Client.Status()}
}

type failingStatusWriter struct {
	client.SubResourceWriter
}

func (failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("injected status write failure")
}

// TestFailedStatusWriteIsMadeByTheNextPass covers the other side of keeping
// the probe schedule out of the status: the pass after a probe is not due to
// probe again, so if the probe's status write failed, that pass has to write
// the outcome from what the controller remembers. Otherwise the status keeps
// the old outcome until the next probe, and keeps an old message for good.
func TestFailedStatusWriteIsMadeByTheNextPass(t *testing.T) {
	f := newProbeOutcomeFixture(t, "tpool-probe-write-retry", "tf-probe-write-retry", "203.0.113.97", 53000)

	const failure = "cannot reach the relay listener"
	f.prober.result = failingProbe(probe.ReasonListenerUnreachable, failure)
	working := f.reconciler.Client
	f.reconciler.Client = statusWriteFailure{Client: working}
	f.clock.Advance(f.interval)
	if _, err := f.reconcileResult(f.name); err == nil {
		t.Fatal("the pass whose status write failed reported no error")
	}
	failedAt := f.clock.Now()
	f.reconciler.Client = working

	calls := len(f.prober.calls)
	f.reconcile(f.name)
	if len(f.prober.calls) != calls {
		t.Fatalf("the pass right after a probe ran %d probe(s), want none: it is not due", len(f.prober.calls)-calls)
	}
	f.expectOutcome("after the pass that followed the failed write", false, failure, failedAt)
}
