//go:build e2e

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package e2e runs Turnfleet against a real kind cluster with a real coturn
// data plane.
//
// The suite is deliberately end to end: it creates TURNPools, waits for the
// operator to create relay pods on the cluster's relay nodes, and then talks to
// those relays with a real TURN client. Nothing is mocked, because the failure
// this project exists to remove — signalling succeeds, the relay candidate never
// forms, every pod looks healthy — is invisible to a mock.
//
// Prerequisites (all of them `make test-e2e` step by step): the kind cluster
// from hack/kind-config.yaml with three relay-labelled workers, the coturn image
// loaded into it, and the operator deployed.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/controller"
	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// Environment the suite reads. The Makefile sets all of them.
const (
	envKubeContext = "KUBE_CONTEXT"
	envKindCluster = "KIND_CLUSTER"
	envCoturnImage = "COTURN_IMAGE"
	envProbeBinary = "PROBE_BIN"

	defaultKubeContext = "kind-turnfleet"
	defaultKindCluster = "turnfleet"
	defaultCoturnImage = "coturn/coturn:4.18.0-r0"
	defaultProbeBinary = "bin/turnfleet-probe"
)

// testNamespace holds every object the suite creates.
const testNamespace = "turnfleet-e2e"

// sharedSecretKey is the key inside the shared HMAC secret, and the value every
// pool in the suite authenticates with.
const (
	//nolint:gosec // the name of a fixture object, not a credential.
	sharedSecretName = "turn-shared-secret"
	sharedSecretKey  = "secret"
)

var (
	kubeContext = env(envKubeContext, defaultKubeContext)
	kindCluster = env(envKindCluster, defaultKindCluster)
	coturnImage = env(envCoturnImage, defaultCoturnImage)
	probeBinary = env(envProbeBinary, defaultProbeBinary)

	k8sClient client.Client
	// sharedSecret is the HMAC secret the credential issuer would also hold.
	sharedSecret string
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// relayNode names a kind worker: kind names containers after the nodes, so the
// node names the planner sees are the cluster name plus the worker suffix.
func relayNode(suffix string) string { return kindCluster + suffix }

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(turnfleetv1alpha1.AddToScheme(scheme))

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading the kubeconfig for context %s: %v\n", kubeContext, err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building the cluster client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := ensureNamespace(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "preparing the test namespace: %v\n", err)
		os.Exit(1)
	}
	if err := ensureSecret(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "preparing the shared secret: %v\n", err)
		os.Exit(1)
	}

	// Start from a known pool state: remove the pools this run did not create
	// before the first case runs.
	if err := resetPoolState(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "resetting the cluster's pool state: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Leave nothing behind: a leftover pool would keep relays bound to node
	// ports and break the next run.
	cleanupPools(context.Background())
	os.Exit(code)
}

// pollInterval is how often the suite rechecks a condition. Every wait has an
// explicit timeout; nothing synchronises with a sleep.
const pollInterval = 500 * time.Millisecond

// waitFor polls until the condition holds, the timeout expires, or the parent
// context is cancelled.
func waitFor(parent context.Context, t *testing.T, what string, timeout time.Duration, condition func(ctx context.Context) (bool, error)) {
	t.Helper()

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			// The deadline is reached: let the poll loop report the timeout
			// instead of letting a helper fail hard on an expired context.
			//nolint:nilerr // reporting the context error here would abort the poll instead of letting it time out.
			return false, nil
		}
		ok, err := condition(ctx)
		if err != nil {
			// A transient API error is not a failed assertion.
			t.Logf("waiting for %s: %v", what, err)
			return false, nil
		}
		return ok, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for %s: %v", what, err)
	}
}

func ensureNamespace(ctx context.Context) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if err := k8sClient.Create(ctx, namespace); err != nil && !isAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureSecret(ctx context.Context) error {
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: sharedSecretName}, secret)
	if err == nil {
		sharedSecret = string(secret.Data[sharedSecretKey])
		return nil
	}

	//nolint:gosec // a fixture value for a throwaway cluster, not a credential.
	sharedSecret = "turnfleet-e2e-shared-secret"
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{sharedSecretKey: []byte(sharedSecret)},
	}
	if err := k8sClient.Create(ctx, secret); err != nil && !isAlreadyExists(err) {
		return err
	}
	return nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// poolRemovalTimeout bounds each wait in the pool cleanup, so a pool that
// cannot be removed fails the run visibly instead of hanging it.
const poolRemovalTimeout = 3 * time.Minute

// cleanupPools deletes the pools the run created. It is best effort — the
// run's result is already decided — but bounded, and it must not hang the
// teardown on a pool that cannot drain.
func cleanupPools(ctx context.Context) {
	if err := removePools(ctx, "the run that created it is over", client.InNamespace(testNamespace)); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: cleaning up after the run: %v\n", err)
	}
}

// resetPoolState removes every TURNPool the cluster already holds, so the run
// starts from a known state instead of from whatever the previous run left.
//
// "Did not create" is defined by the moment, not the name. The suite creates
// every pool it owns as e2e-<case> in the turnfleet-e2e namespace, and this
// runs before the first case, so no pool present now can be this run's. The
// naming cannot separate this run's pools from a previous run's either: a
// previous run used the same names, and a failed run leaves exactly those
// pools behind. So any pool present here is a leftover to remove: a stray
// applied by hand (config/samples/turnfleet_v1alpha1_turnpool.yaml) or an
// e2e-* pool an earlier run failed to delete.
//
// The reason is not tidiness. A pool holds a relay address and a port range;
// a leftover pool bound to the node address the suite's own pool also uses
// leaves the two sharing one address, which the operator reports as a degraded
// state, so the suite would fail for a reason that is in that earlier run and
// not in its own diff. The suite owns KIND_CLUSTER — make kind-up and make
// kind-down manage it — so every pool on it is the suite's to clear.
func resetPoolState(ctx context.Context) error {
	return removePools(ctx, "it holds a relay address and a port range the suite's pools would share")
}

// removePools deletes every pool the list options select and waits, bounded,
// for them to go.
//
// A TURNPool carries a drain finalizer (controller.FinalizerName) that only a
// running controller removes: a pool deleted while the controller is down —
// the case here, a leftover from an earlier failed run — would sit in
// Terminating forever, and anything waiting on it would block. So if a pool is
// still there after the bounded wait and still carries that finalizer, strip
// it and delete again, then wait once more. Nothing else removes it.
func removePools(ctx context.Context, reason string, listOpts ...client.ListOption) error {
	selected := func(ctx context.Context) (*turnfleetv1alpha1.TURNPoolList, error) {
		list := &turnfleetv1alpha1.TURNPoolList{}
		if err := k8sClient.List(ctx, list, listOpts...); err != nil {
			return nil, err
		}
		return list, nil
	}

	pools, err := selected(ctx)
	if err != nil {
		return fmt.Errorf("listing pools to remove: %w", err)
	}
	for i := range pools.Items {
		leftover := &pools.Items[i]
		// Log what is removed and why.
		fmt.Fprintf(os.Stderr, "e2e: removing pool %s/%s: %s\n", leftover.Namespace, leftover.Name, reason)
		if err := k8sClient.Delete(ctx, leftover); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pool %s/%s: %w", leftover.Namespace, leftover.Name, err)
		}
	}
	if len(pools.Items) == 0 {
		return nil
	}

	empty := func(ctx context.Context) (bool, error) {
		list, err := selected(ctx)
		if err != nil {
			//nolint:nilerr // a transient API error is not a stuck pool: keep polling until the deadline.
			return false, nil
		}
		return len(list.Items) == 0, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, poolRemovalTimeout, true, empty); err == nil {
		return nil
	}

	stragglers, err := selected(ctx)
	if err != nil {
		return fmt.Errorf("listing pools to strip: %w", err)
	}
	for i := range stragglers.Items {
		leftover := &stragglers.Items[i]
		if !slices.Contains(leftover.Finalizers, controller.FinalizerName) {
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e: stripping the drain finalizer from pool %s/%s: no controller is running to drain it\n", leftover.Namespace, leftover.Name)
		patch := client.MergeFrom(leftover.DeepCopy())
		leftover.Finalizers = slices.DeleteFunc(leftover.Finalizers, func(name string) bool { return name == controller.FinalizerName })
		if err := k8sClient.Patch(ctx, leftover, patch); err != nil {
			return fmt.Errorf("stripping the drain finalizer from pool %s/%s: %w", leftover.Namespace, leftover.Name, err)
		}
		if err := k8sClient.Delete(ctx, leftover); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pool %s/%s again: %w", leftover.Namespace, leftover.Name, err)
		}
	}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, poolRemovalTimeout, true, empty); err != nil {
		return fmt.Errorf("waiting for pools to be removed: %w", err)
	}
	return nil
}

// metricsServiceName is the Service in front of the controller's metrics
// endpoint (config/default/metrics_service.yaml).
const metricsServiceName = "turnfleet-controller-manager-metrics-service"

// relaySecretRef points a pool at the suite's shared secret.
func relaySecretRef() *corev1.SecretKeySelector {
	ref := &corev1.SecretKeySelector{}
	ref.Name = sharedSecretName
	ref.Key = sharedSecretKey
	return ref
}

// createPool creates a pool and waits until the operator has seen it.
func createPool(t *testing.T, pool *turnfleetv1alpha1.TURNPool) {
	t.Helper()

	pool.Namespace = testNamespace
	if pool.Spec.Dataplane.Image == "" {
		// The cluster has this image loaded (make load-coturn) and polls it with
		// IfNotPresent. Digest pinning is enforced by the validating webhook and
		// covered by the webhook envtest suite; here the point is the data plane.
		pool.Spec.Dataplane.Image = coturnImage
	}
	if pool.Spec.Auth.Realm == "" {
		pool.Spec.Auth.Realm = "turnfleet.e2e"
	}
	if pool.Spec.Auth.SecretRef == nil {
		pool.Spec.Auth.SecretRef = relaySecretRef()
	}
	if pool.Spec.PeerPolicy == nil {
		pool.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{DeniedCIDRs: relay.DefaultDeniedCIDRs()}
	}

	if err := k8sClient.Create(context.Background(), pool); err != nil {
		t.Fatalf("creating pool %s: %v", pool.Name, err)
	}
	t.Cleanup(func() { deletePool(t, pool.Name) })
}

// deletePool deletes a pool and waits for its relays to be gone, so the next
// test can reuse the node ports.
func deletePool(t *testing.T, name string) {
	t.Helper()

	ctx := context.Background()
	pool := &turnfleetv1alpha1.TURNPool{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, pool); err != nil {
		return
	}
	if err := k8sClient.Delete(ctx, pool); err != nil {
		t.Logf("deleting pool %s: %v", name, err)
		return
	}
	waitFor(ctx, t, "pool "+name+" to be deleted", 3*time.Minute, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &turnfleetv1alpha1.TURNPool{})
		return err != nil, nil
	})
}

// pool reads a pool's current state.
func pool(ctx context.Context, t *testing.T, name string) *turnfleetv1alpha1.TURNPool {
	t.Helper()

	got := &turnfleetv1alpha1.TURNPool{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, got); err != nil {
		t.Fatalf("reading pool %s: %v", name, err)
	}
	return got
}

// relayPods lists a pool's relay pods.
func relayPods(ctx context.Context, t *testing.T, poolName string) []corev1.Pod {
	t.Helper()

	pods := &corev1.PodList{}
	selector := controller.SelectorLabels(&turnfleetv1alpha1.TURNPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}})
	if err := k8sClient.List(ctx, pods,
		client.InNamespace(testNamespace), client.MatchingLabels(selector)); err != nil {
		t.Fatalf("listing relay pods of %s: %v", poolName, err)
	}
	return pods.Items
}

// waitForRelays waits until a pool reports the wanted number of ready relays.
func waitForRelays(ctx context.Context, t *testing.T, poolName string, want int32) *turnfleetv1alpha1.TURNPool {
	t.Helper()

	waitFor(ctx, t, fmt.Sprintf("%s to reach %d ready relays", poolName, want), 4*time.Minute, func(ctx context.Context) (bool, error) {
		current := pool(ctx, t, poolName)
		if current.Status == nil || current.Status.ReadyRelays == nil {
			return false, nil
		}
		return *current.Status.ReadyRelays == want, nil
	})
	return pool(ctx, t, poolName)
}

// relayStatus finds one relay's status entry.
func relayStatus(ctx context.Context, t *testing.T, poolName, relayName string) turnfleetv1alpha1.RelayStatus {
	t.Helper()

	for _, status := range pool(ctx, t, poolName).Status.Relays {
		if status.Name == relayName {
			return status
		}
	}
	t.Fatalf("relay %s is not in the status of pool %s", relayName, poolName)
	return turnfleetv1alpha1.RelayStatus{}
}

// probeRelay performs a real TURN Allocate against a relay, the way an external
// client would: from this process, which runs on the host, over the network.
func probeRelay(t *testing.T, address string, port int32, portRange relay.PortRange) probe.Result {
	t.Helper()

	username, password := probe.Credentials(sharedSecret, probe.DefaultUser, probe.DefaultCredentialTTL, time.Now())
	return probe.Allocate(context.Background(), &probe.Config{
		Endpoint:        probe.Endpoint{Host: address, Port: port, Transport: probe.TransportUDP},
		Username:        username,
		Password:        password,
		Timeout:         5 * time.Second,
		ExpectAddress:   address,
		ExpectPortRange: &portRange,
	})
}

// relayListenerHostPort reads the address and listener port a relay publishes in
// its turnURIs: that is exactly what a client is handed, so the suite probes what
// a client would use rather than a number it computed itself.
func relayListenerHostPort(t *testing.T, status *turnfleetv1alpha1.RelayStatus) (host string, port int32) {
	t.Helper()

	if len(status.TurnURIs) == 0 {
		t.Fatalf("relay %s publishes no turnURIs", status.Name)
	}
	uri := status.TurnURIs[0]
	for _, scheme := range []string{"turn:", "turns:"} {
		if !strings.HasPrefix(uri, scheme) {
			continue
		}
		hostPort, _, _ := strings.Cut(strings.TrimPrefix(uri, scheme), "?")
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			t.Fatalf("relay %s published an unreadable URI %q: %v", status.Name, uri, err)
		}
		parsed, err := strconv.ParseInt(port, 10, 32)
		if err != nil {
			t.Fatalf("relay %s published an unreadable port in %q: %v", status.Name, uri, err)
		}
		return host, int32(parsed)
	}
	t.Fatalf("relay %s published an unsupported URI %q", status.Name, uri)
	return "", 0
}

// kubectl runs kubectl against the kind cluster and returns its output.
//
// It bounds the call itself rather than inheriting a caller's context: a helper
// that is polled must not be killed mid-flight by the poll's deadline, or the
// failure reads as "signal: killed" instead of the condition it was checking.
func kubectl(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()

	// The call is detached from the caller's cancellation and given its own
	// deadline: a helper that is polled must not be killed mid-flight by the
	// poll's deadline, because that reads as "signal: killed" instead of the
	// condition the caller was checking.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kubectlTimeout)
	defer cancel()

	full := append([]string{"--context", kubeContext}, args...)
	//nolint:gosec // the suite execs the tools it documents; the arguments come from the tests, not from untrusted input.
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// kubectlTimeout bounds one kubectl call.
const kubectlTimeout = 30 * time.Second

// hostProbe runs the probe CLI from the host and parses its JSON output. It
// also returns how long the process took, so a caller can hold the probe's own
// latency against the clock it was observed with.
func hostProbe(ctx context.Context, t *testing.T, uri string, extraArgs ...string) (probeCLIResult, time.Duration) {
	t.Helper()

	secretFile := writeSecretFile(t)
	args := append([]string{
		"--uri", uri,
		"--secret-file", secretFile,
		"--json",
	}, extraArgs...)

	//nolint:gosec // the binary path is a repository-local build artifact.
	cmd := exec.CommandContext(ctx, probeBinary, args...)
	started := time.Now()
	output, err := cmd.Output()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("turnfleet-probe %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}

	var result probeCLIResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("turnfleet-probe did not return valid JSON: %v\n%s", err, output)
	}
	return result, elapsed
}

// writeSecretFile writes the shared secret to a temporary file, the way an
// operator would hand it to the CLI.
func writeSecretFile(t *testing.T) string {
	t.Helper()

	path := t.TempDir() + "/turn-secret"
	if err := os.WriteFile(path, []byte(sharedSecret), 0o600); err != nil {
		t.Fatalf("writing the secret file: %v", err)
	}
	return path
}

// probeCLIResult is the CLI's JSON output, decoded loosely: the suite asserts on
// the fields a consumer would read.
type probeCLIResult struct {
	Succeeded      bool   `json:"succeeded"`
	URI            string `json:"uri"`
	RelayedAddress string `json:"relayedAddress"`
	RelayedPort    int32  `json:"relayedPort"`
	ErrorCode      int    `json:"errorCode"`
	Message        string `json:"message"`
	LatencyMillis  int64  `json:"latencyMillis"`
}
