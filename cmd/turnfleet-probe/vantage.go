// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Hazmi35/turnfleet/internal/probe"
	"github.com/Hazmi35/turnfleet/internal/vantage"
)

// serviceAccountNamespaceFile is where a pod reads its own namespace, so a
// runner in the cluster does not have to be told which namespace to publish in.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// runVantage probes every listener URI the pool publishes and writes the result
// where the controller reads it.
//
// This is the runner half of the delivery path (docs/adr/0015-vantage-result-delivery.md):
// the same check the controller runs, run from a vantage point the controller
// cannot occupy, with its outcome published rather than kept in memory.
func runVantage(ctx context.Context, opts *options, stdout, stderr io.Writer) int {
	secret, err := readSecret(opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid arguments: %v\n", err)
		return exitUsage
	}
	if secret == "" {
		_, _ = fmt.Fprintln(stderr, "the shared secret is empty")
		return exitUsage
	}

	namespace := resolveNamespace(opts.namespace)
	cluster, err := clusterClient()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}

	uris := []string{opts.uri}
	if opts.urisConfigmap != "" {
		uris, err = publishedURIs(ctx, cluster, namespace, opts.urisConfigmap)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return exitFailure
		}
	}

	username, password := probe.Credentials(secret, opts.user, opts.ttl, time.Now())
	results := make([]probe.Result, 0, len(uris))
	for _, uri := range uris {
		results = append(results, probeListener(ctx, opts, uri, username, password))
	}

	payload := &vantage.Payload{
		Pool:       opts.pool,
		ObservedAt: time.Now(),
		Results:    results,
	}

	// Publish before reporting: a failing relay must reach the pool's status
	// whether or not this process exits non-zero afterwards.
	if opts.publishConfigmap != "" {
		if err := publishResult(ctx, cluster, namespace, opts.publishConfigmap, payload); err != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return exitFailure
		}
		_, _ = fmt.Fprintf(stderr, "published %d result(s) in %s/%s\n", len(results), namespace, opts.publishConfigmap)
	}

	if err := writeVantageResult(stdout, payload, opts.jsonOutput); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return exitFailure
	}
	if len(payload.Failures()) > 0 {
		return exitFailure
	}
	return exitOK
}

// validateVantageOptions rejects flag combinations that would silently change
// what is probed.
func validateVantageOptions(opts *options) error {
	switch {
	case opts.uri != "" && opts.urisConfigmap != "":
		return errors.New("use --uri or --uris-configmap, not both")
	case opts.urisConfigmap != "" && opts.expectAddress != "":
		return errors.New("--expect-address does not apply to a list of URIs: each URI names the address its relay must advertise, which is what the probe expects")
	case opts.urisConfigmap != "" && opts.expectPortRange != "":
		return errors.New("--expect-port-range does not apply to a list of URIs: a runner reads the pool's URI list, not its spec, so it does not know each relay's range")
	case opts.urisConfigmap != "" && opts.peerAddress != "":
		return errors.New("--peer-address does not apply to a list of URIs: use --peer-echo")
	case opts.publishConfigmap != "" && opts.pool == "":
		return errors.New("--pool is required with --publish-configmap: the published result names the pool it is about")
	case opts.publishConfigmap == "" && opts.pool != "":
		return errors.New("--pool only applies with --publish-configmap")
	}
	return nil
}

// resolveNamespace finds the namespace the pool lives in: the flag, then the
// pod's own namespace, then the kubeconfig's default.
func resolveNamespace(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	// A pod always has this file; a developer's machine does not.
	if data, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if namespace := strings.TrimSpace(string(data)); namespace != "" {
			return namespace
		}
	}
	return metav1.NamespaceDefault
}

// clusterClient connects to the API server: in-cluster from the pod's service
// account, or from KUBECONFIG when the runner is somewhere else.
func clusterClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the Kubernetes configuration: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building the client scheme: %w", err)
	}
	cluster, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("connecting to the API server: %w", err)
	}
	return cluster, nil
}

// publishedURIs reads the listener URIs the pool publishes for its clients.
//
// An empty list is not an error: a pool with no ready relay publishes none, and
// the runner reports that it had nothing to probe rather than pretending the
// pool was reachable.
func publishedURIs(ctx context.Context, cluster client.Client, namespace, name string) ([]string, error) {
	configMap := &corev1.ConfigMap{}
	if err := cluster.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, configMap); err != nil {
		return nil, fmt.Errorf("reading configmap %s/%s: %w", namespace, name, err)
	}

	var uris []string
	for line := range strings.SplitSeq(configMap.Data["uris"], "\n") {
		if uri := strings.TrimSpace(line); uri != "" {
			uris = append(uris, uri)
		}
	}
	return uris, nil
}

// probeListener probes one listener URI.
//
// The expectation is the address the URI names: from a vantage point that is
// the only thing that can be asserted without reading the pool's spec, and it
// is what catches a relay advertising an address clients cannot use.
func probeListener(ctx context.Context, opts *options, uri, username, password string) probe.Result {
	endpoint, err := probe.ParseURI(uri)
	if err != nil {
		return probe.Result{URI: uri, Message: fmt.Sprintf("unreadable TURN URI in the pool's list: %v", err)}
	}
	return probe.Allocate(ctx, &probe.Config{
		Endpoint:      endpoint,
		Username:      username,
		Password:      password,
		Timeout:       opts.timeout,
		ExpectAddress: endpoint.Host,
		EchoListen:    opts.peerEcho,
		Logger:        stderrLogger{},
	})
}

// publishResult writes the payload where the controller reads it.
//
// The object carries no owner reference on purpose: the runner does not own the
// pool, and the controller deletes the result when it deletes the pool.
func publishResult(ctx context.Context, cluster client.Client, namespace, name string, payload *vantage.Payload) error {
	data, err := payload.Data()
	if err != nil {
		return err
	}

	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, cluster, configMap, func() error {
		configMap.Labels = vantage.Labels(payload.Pool)
		configMap.Data = data
		return nil
	}); err != nil {
		return fmt.Errorf("writing configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}

// writeVantageResult reports the pass: the payload as JSON, or a line per
// listener URI.
func writeVantageResult(stdout io.Writer, payload *vantage.Payload, asJSON bool) error {
	if asJSON {
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding the result: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", encoded)
		return nil
	}

	for i := range payload.Results {
		writeHumanResult(stdout, &payload.Results[i])
	}
	_, _ = fmt.Fprintf(stdout, "%d of %d listener URI(s) answered\n",
		len(payload.Results)-len(payload.Failures()), len(payload.Results))
	return nil
}
