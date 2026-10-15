// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/metrics"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	testScheme = runtime.NewScheme()
)

// fixtureCounter numbers the test namespaces. A counter and not a clock
// reading: `time.Now().UnixNano() % 1_000_000` repeats every millisecond, so
// two fixtures could pick the same name and the second Create would fail with
// AlreadyExists — a failure with nothing to do with the code under test.
var fixtureCounter atomic.Uint64

func TestMain(m *testing.M) {
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(turnfleetv1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run the tests through 'make test'")
		os.Exit(1)
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building the test client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	_ = testEnv.Stop()
	os.Exit(code)
}

// secretName is the shared HMAC secret every fixture pool points at, and
// fixtureSecret is its value.
const (
	secretName    = "turn-secret"
	fixtureSecret = "0123456789abcdef"
)

// ctrlRequest builds the reconcile request for a pool.
func ctrlRequest(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// fakeClock is a clock the tests move by hand, so drain timeouts and probe
// intervals are exercised without sleeping.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// testProber records the probes it was asked for and answers with whatever the
// test configured.
type testProber struct {
	calls  []*probe.Config
	result *probe.Result
}

func (p *testProber) probe(_ context.Context, cfg *probe.Config) probe.Result {
	p.calls = append(p.calls, cfg)
	if p.result != nil {
		return *p.result
	}
	return probe.Result{
		Succeeded:      true,
		URI:            cfg.Endpoint.String(),
		RelayedAddress: cfg.ExpectAddress,
		RelayedPort:    cfg.ExpectPortRange.Start,
		Message:        "relay allocated and advertised the expected relayed address",
	}
}

// testScraper answers metric scrapes with a fixed allocation count.
type testScraper struct {
	allocations int32
	fail        bool
}

func (s *testScraper) scrape(_ context.Context, _ string) (metrics.CoturnSample, error) {
	if s.fail {
		return metrics.CoturnSample{}, fmt.Errorf("scrape failed")
	}
	return metrics.CoturnSample{Allocations: s.allocations, Samples: map[string]float64{}}, nil
}

// testFixture is one controller test's world.
type testFixture struct {
	t          *testing.T
	ctx        context.Context
	reconciler *TURNPoolReconciler
	clock      *fakeClock
	prober     *testProber
	scraper    *testScraper
	namespace  string
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()

	clock := newFakeClock()
	prober := &testProber{}
	scraper := &testScraper{}

	namespace := fmt.Sprintf("turnfleet-%d", fixtureCounter.Add(1))
	ctx := context.Background()
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("creating the test namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	})

	return &testFixture{
		t:   t,
		ctx: ctx,
		reconciler: &TURNPoolReconciler{
			Client:   k8sClient,
			Scheme:   testScheme,
			Recorder: events.NewFakeRecorder(100),
			Now:      clock.Now,
			Prober:   prober.probe,
			Scraper:  scraper.scrape,
		},
		clock:     clock,
		prober:    prober,
		scraper:   scraper,
		namespace: namespace,
	}
}

// reconcile runs one pass of the loop.
func (f *testFixture) reconcile(name string) {
	f.t.Helper()

	_, err := f.reconciler.Reconcile(f.ctx, ctrlRequest(f.namespace, name))
	if err != nil {
		f.t.Fatalf("Reconcile(%s) failed: %v", name, err)
	}
}

// reconcileAttempts bounds reconcileUntil. A condition that does not hold by
// then fails the test instead of hanging it.
const reconcileAttempts = 80

// reconcileUntil runs the loop until the condition holds, advancing the fake
// clock between passes. It fails the test instead of hanging.
func (f *testFixture) reconcileUntil(name, what string, condition func() bool) {
	f.t.Helper()

	for range reconcileAttempts {
		f.reconcile(name)
		// envtest runs no kubelet, so the test makes new pods look ready.
		f.markPodsReady(name)
		if condition() {
			return
		}
		f.clock.Advance(time.Second)
	}
	f.t.Fatalf("condition %q never held after %d reconciles", what, reconcileAttempts)
}

func (f *testFixture) pool(name string) *turnfleetv1alpha1.TURNPool {
	f.t.Helper()

	pool := &turnfleetv1alpha1.TURNPool{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, pool); err != nil {
		f.t.Fatalf("getting pool %s: %v", name, err)
	}
	return pool
}

func (f *testFixture) pods(pool string) []corev1.Pod {
	f.t.Helper()

	list := &corev1.PodList{}
	if err := k8sClient.List(f.ctx, list, client.InNamespace(f.namespace),
		client.MatchingLabels(SelectorLabels(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: pool}}))); err != nil {
		f.t.Fatalf("listing relay pods: %v", err)
	}
	return list.Items
}

// relayConfig returns the rendered coturn configuration of a pool's relays, as
// the controller published it in the pool's relay Secret.
func (f *testFixture) relayConfig(pool string) string {
	f.t.Helper()

	name := RelayConfigSecretName(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: pool}})
	secret := &corev1.Secret{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, secret); err != nil {
		f.t.Fatalf("getting secret %s: %v", name, err)
	}
	if len(secret.Data) == 0 {
		f.t.Fatalf("secret %s holds no relay configuration", name)
	}

	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	rendered := make([]string, 0, len(keys))
	for _, key := range keys {
		rendered = append(rendered, string(secret.Data[key]))
	}
	return strings.Join(rendered, "\n")
}

// markPodsReady makes every relay pod look like a running, ready pod with an
// address. envtest runs no kubelet, so the test plays that part.
func (f *testFixture) markPodsReady(pool string) {
	f.t.Helper()

	// One List for the whole call: re-listing inside the loop cost one API
	// round trip per pod, and reconcileUntil can call this 80 times.
	pods := f.pods(pool)
	for i := range pods {
		pod := pods[i]
		patch := client.MergeFrom(pod.DeepCopy())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "10.244.0.10"
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
		if err := k8sClient.Status().Patch(f.ctx, &pod, patch); err != nil {
			f.t.Fatalf("marking pod %s ready: %v", pod.Name, err)
		}
	}
}

// createPodIPNode creates a relay node that declares the addresses a PodIP
// pool plans on. The node has no relay address of its own: in this mode the
// relay address belongs to the pod, and the node only declares which addresses
// are reserved for the pool's relays.
func (f *testFixture) createPodIPNode(name string, declared ...string) {
	f.t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{RelayNodeLabel: "true"},
			Annotations: map[string]string{relay.PodAddressesAnnotation: strings.Join(declared, ",")},
		},
		Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/24"},
	}
	if err := k8sClient.Create(f.ctx, node); err != nil {
		f.t.Fatalf("creating node %s: %v", name, err)
	}
	f.t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
}

// createNode creates a relay node with an address.
func (f *testFixture) createNode(name, address string) {
	f.t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{RelayNodeLabel: "true"},
		},
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: address}}
	node.Spec.PodCIDR = "10.244.0.0/24"

	if err := k8sClient.Create(f.ctx, node); err != nil {
		f.t.Fatalf("creating node %s: %v", name, err)
	}
	node.Status = corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: address}},
	}
	if err := k8sClient.Status().Update(f.ctx, node); err != nil {
		f.t.Fatalf("setting the address of node %s: %v", name, err)
	}
	f.t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
}

// createDualStackNode creates a relay node with one address per family, the way
// a dual-stack cluster reports them.
func (f *testFixture) createDualStackNode(name, v4, v6 string) {
	f.t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{RelayNodeLabel: "true"},
		},
	}
	node.Spec.PodCIDR = "10.244.0.0/24"

	if err := k8sClient.Create(f.ctx, node); err != nil {
		f.t.Fatalf("creating node %s: %v", name, err)
	}
	node.Status = corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: v4},
			{Type: corev1.NodeExternalIP, Address: v4},
			{Type: corev1.NodeInternalIP, Address: v6},
			{Type: corev1.NodeExternalIP, Address: v6},
		},
	}
	if err := k8sClient.Status().Update(f.ctx, node); err != nil {
		f.t.Fatalf("setting the addresses of node %s: %v", name, err)
	}
	f.t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
}

// createSecret creates the pool's shared HMAC secret, under the name every
// fixture pool refers to.
func (f *testFixture) createSecret() {
	f.t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: f.namespace},
		Data:       map[string][]byte{"secret": []byte(fixtureSecret)},
	}
	if err := k8sClient.Create(f.ctx, secret); err != nil {
		f.t.Fatalf("creating secret %s: %v", secretName, err)
	}
}

// rotateSecret changes the shared secret's value, which must roll the relays.
func (f *testFixture) rotateSecret(value string) {
	f.t.Helper()

	secret := &corev1.Secret{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: secretName}, secret); err != nil {
		f.t.Fatalf("getting secret %s: %v", secretName, err)
	}
	secret.Data["secret"] = []byte(value)
	if err := k8sClient.Update(f.ctx, secret); err != nil {
		f.t.Fatalf("rotating secret %s: %v", secretName, err)
	}
}

// createTLSSecret creates the serving certificate a TLS listener mounts.
func (f *testFixture) createTLSSecret(name, certificate string) {
	f.t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace},
		Data: map[string][]byte{
			"tls.crt": []byte(certificate),
			"tls.key": []byte("key-for-" + certificate),
		},
	}
	if err := k8sClient.Create(f.ctx, secret); err != nil {
		f.t.Fatalf("creating tls secret %s: %v", name, err)
	}
}

// rotateTLSSecret replaces the certificate, which must roll the relays.
func (f *testFixture) rotateTLSSecret(name, certificate string) {
	f.t.Helper()

	secret := &corev1.Secret{}
	if err := k8sClient.Get(f.ctx, types.NamespacedName{Namespace: f.namespace, Name: name}, secret); err != nil {
		f.t.Fatalf("getting tls secret %s: %v", name, err)
	}
	secret.Data["tls.crt"] = []byte(certificate)
	if err := k8sClient.Update(f.ctx, secret); err != nil {
		f.t.Fatalf("rotating tls secret %s: %v", name, err)
	}
}

// createPool creates a valid pool in the fixture's namespace.
func (f *testFixture) createPool(pool *turnfleetv1alpha1.TURNPool) {
	f.t.Helper()

	pool.Namespace = f.namespace
	if pool.Spec.Dataplane.Image == "" {
		pool.Spec.Dataplane.Image = "coturn/coturn:4.18.0-r0@sha256:" +
			"bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3"
	}
	if pool.Spec.Auth.Realm == "" {
		pool.Spec.Auth.Realm = "turnfleet.test"
	}
	if pool.Spec.Auth.SecretRef == nil {
		pool.Spec.Auth.SecretRef = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  "secret",
		}
	}
	// No peer policy is injected here on purpose: the defaulting webhook is not
	// installed in this suite, so the controller must work with whatever peer
	// policy the spec carries, including none at all.
	if len(pool.Spec.Listeners) == 0 {
		pool.Spec.Listeners = []turnfleetv1alpha1.ListenerSpec{
			{Name: "turn", Protocol: turnfleetv1alpha1.ListenerProtocolUDP, Port: 3478},
		}
	}
	if err := k8sClient.Create(f.ctx, pool); err != nil {
		f.t.Fatalf("creating pool %s: %v", pool.Name, err)
	}
}
