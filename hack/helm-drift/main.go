// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Command helm-drift verifies that charts/turnfleet installs the same controller
// that config/ deploys.
//
// config/default and config/with-webhook stay the source of truth for what the
// controller is; the chart installs the same objects. This guard renders both
// and fails on drift, so a change to config/ that the chart does not follow
// fails `make ci` instead of leaving a chart that quietly installs something
// else.
//
// What it compares, and why it is not a byte diff:
//
//   - Objects, by (apiVersion, kind, namespace, name). A missing or extra object
//     is reported as such, so a resource added to config/default cannot be
//     silently absent from the chart.
//   - Every field of an object that both sides render, after both are decoded
//     into JSON with sorted keys, so nothing depends on key order or on how the
//     YAML is wrapped.
//   - Namespace objects are skipped: kustomize creates the namespace as a
//     resource, Helm creates it with `--create-namespace`, so the chart has no
//     Namespace template for the overlay's to match.
//   - Top-level `metadata.labels` are skipped, and in the label maps that remain
//     (the pod template's) the installer's keys are skipped:
//     `app.kubernetes.io/managed-by`, `app.kubernetes.io/instance`,
//     `app.kubernetes.io/version` and `helm.sh/*`. These name the installer and
//     the release rather than the deployment — kustomize writes
//     `managed-by: kustomize`, Helm writes the release name, the chart version
//     and `managed-by: Helm` — and config/ itself is inconsistent about which
//     objects carry labels at all (config/rbac/role.yaml has none,
//     config/manager/manager.yaml has three, so the chart cannot match it per
//     object without copying that inconsistency). The labels the cluster acts on
//     are inside a spec — the Deployment's selector, its pod template and the
//     Services' selectors — and those are compared in full.
//   - Empty maps and lists: `volumeMounts: []` and no volumeMounts are the same
//     deployment, and config/manager/manager.yaml carries those placeholders.
//   - `meta.helm.sh/*` annotations, which Helm adds and kustomize does not.
//
// The manager's image is rendered from the same value on both sides: config/
// carries `controller:latest`, the builder's placeholder that the Makefile
// replaces on the cluster after applying it, so the guard renders the chart with
// that placeholder. The chart's own default image, and the image tag `make
// helm-install` passes, are covered by the chart's end-to-end install, which
// builds an image and runs a relay from it.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/yaml"
)

// chartRelease is the release name the guard renders the chart under. The
// chart's object names come from its `nameOverride` value, not from the release
// name, so the release only has to be stable, not meaningful.
const chartRelease = "turnfleet"

// chartNamespace is the namespace config/default deploys into. The chart takes
// its namespace from the release, so rendering there is what makes the two
// comparable.
const chartNamespace = "turnfleet-system"

// installerLabelKeys name the installer or the release, not the deployment.
var installerLabelKeys = []string{
	"app.kubernetes.io/managed-by",
	"app.kubernetes.io/instance",
	"app.kubernetes.io/version",
}

// setFlag is the Helm flag that carries a value, which the guard uses to render
// a case with the values that make the chart match an overlay.
const setFlag = "--set"

// placeholderImage is the image config/manager/manager.yaml carries: the
// builder's placeholder, which the Makefile replaces on the cluster with the
// image it just built.
var placeholderImage = []string{
	setFlag, "image.repository=controller",
	setFlag, "image.tag=latest",
}

// driftCase is one overlay and the values that make the chart render it.
type driftCase struct {
	name    string
	overlay string
	values  []string
}

func main() {
	helmPath := flag.String("helm", "bin/helm", "the pinned helm binary")
	kustomizePath := flag.String("kustomize", "bin/kustomize", "the pinned kustomize binary")
	chartPath := flag.String("chart", "charts/turnfleet", "the chart to render")
	flag.Parse()

	cases := []driftCase{
		{name: "config/default", overlay: "config/default"},
		{
			name:    "config/with-webhook",
			overlay: "config/with-webhook",
			values:  []string{setFlag, "webhook.enabled=true"},
		},
	}

	drifted := false
	for _, c := range cases {
		rendered, err := renderChart(*helmPath, *chartPath, append(c.values, placeholderImage...))
		if err != nil {
			fatal(err)
		}
		deployed, err := renderOverlay(*kustomizePath, c.overlay)
		if err != nil {
			fatal(err)
		}

		report, ok := compare(c.name, deployed, rendered)
		if !ok {
			drifted = true
			fmt.Fprint(os.Stderr, report)
			continue
		}
		fmt.Printf("helm-drift: %s matches the chart (%d objects)\n", c.name, len(deployed))
	}

	if drifted {
		fmt.Fprintln(os.Stderr, "helm-drift: the chart and config/ have drifted apart; update charts/turnfleet (see the differences above)")
		os.Exit(1)
	}
}

// renderChart renders the chart and returns its normalized objects.
func renderChart(helm, chart string, values []string) (map[string]object, error) {
	args := make([]string, 0, 6+len(values))
	args = append(args, "template", chartRelease, chart, "--namespace", chartNamespace, "--include-crds")
	args = append(args, values...)

	out, err := run(helm, args...)
	if err != nil {
		return nil, err
	}
	return objects(out)
}

// renderOverlay renders the overlay and returns its normalized objects.
func renderOverlay(kustomize, overlay string) (map[string]object, error) {
	out, err := run(kustomize, "build", overlay)
	if err != nil {
		return nil, err
	}
	return objects(out)
}

func run(binary string, args ...string) ([]byte, error) {
	//nolint:gosec // the binaries are the pinned helm and kustomize, passed by the Makefile target; the arguments are overlay paths and values from this file.
	command := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", binary, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// object is one rendered Kubernetes object.
type object struct {
	// key identifies the object: apiVersion, kind, namespace and name.
	key string
	// json is the normalized object, so two objects can be compared as text.
	json string
	// document is the normalized object pretty-printed, for a readable diff.
	document string
}

// objects decodes a multi-document YAML stream into the objects to compare.
func objects(stream []byte) (map[string]object, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(stream), 4096)
	found := map[string]object{}

	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decoding a rendered document: %w", err)
		}
		if len(raw) == 0 {
			continue
		}

		key, err := objectKey(raw)
		if err != nil {
			return nil, err
		}
		if key == "" {
			// A Namespace: the installer creates it.
			continue
		}

		normalize(raw)

		compact, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("printing %s: %w", key, err)
		}
		pretty, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("printing %s: %w", key, err)
		}
		found[key] = object{key: key, json: string(compact), document: string(pretty)}
	}
	return found, nil
}

// objectKey identifies an object, or returns "" for one that is not compared.
func objectKey(raw map[string]any) (string, error) {
	apiVersion, _ := raw["apiVersion"].(string)
	kind, _ := raw["kind"].(string)
	if apiVersion == "" || kind == "" {
		return "", fmt.Errorf("a rendered document has no apiVersion or kind: %v", raw)
	}
	if kind == "Namespace" {
		return "", nil
	}

	metadata, ok := raw["metadata"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s has no metadata", kind)
	}
	name, _ := metadata["name"].(string)
	if name == "" {
		return "", fmt.Errorf("%s has no name", kind)
	}
	namespace, _ := metadata["namespace"].(string)

	return strings.Join([]string{apiVersion, kind, namespace, name}, "/"), nil
}

// normalize drops the parts that legitimately differ between the two
// installers.
func normalize(raw map[string]any) {
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		delete(metadata, "labels")
	}
	dropInstallerMetadata(raw)
	dropEmpty(raw)
}

// dropInstallerMetadata removes the labels and annotations that name the
// installer, wherever a label or annotation map appears.
func dropInstallerMetadata(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			switch key {
			case "labels":
				if labels, ok := nested.(map[string]any); ok {
					for label := range labels {
						for _, installer := range installerLabelKeys {
							if label == installer {
								delete(labels, label)
							}
						}
						if strings.HasPrefix(label, "helm.sh/") {
							delete(labels, label)
						}
					}
				}
			case "annotations":
				if annotations, ok := nested.(map[string]any); ok {
					for annotation := range annotations {
						if strings.HasPrefix(annotation, "meta.helm.sh/") || strings.HasPrefix(annotation, "helm.sh/") {
							delete(annotations, annotation)
						}
					}
				}
			}
			dropInstallerMetadata(nested)
		}
	case []any:
		for _, nested := range typed {
			dropInstallerMetadata(nested)
		}
	}
}

// dropEmpty removes empty maps and lists, which mean the same thing to the API
// server as an absent key.
func dropEmpty(value any) {
	container, ok := value.(map[string]any)
	if !ok {
		if list, isList := value.([]any); isList {
			for _, nested := range list {
				dropEmpty(nested)
			}
		}
		return
	}

	for key, nested := range container {
		dropEmpty(nested)
		switch typed := nested.(type) {
		case map[string]any:
			if len(typed) == 0 {
				delete(container, key)
			}
		case []any:
			if len(typed) == 0 {
				delete(container, key)
			}
		}
	}
}

// compare reports the difference between the overlay's objects and the chart's,
// and whether they match.
func compare(overlay string, deployed, rendered map[string]object) (string, bool) {
	missing := make([]string, 0, len(deployed))
	extra := make([]string, 0, len(rendered))
	for key := range deployed {
		if _, ok := rendered[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range rendered {
		if _, ok := deployed[key]; !ok {
			extra = append(extra, key)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)

	differing := make([]string, 0, len(deployed))
	for key, want := range deployed {
		got, ok := rendered[key]
		if !ok {
			continue
		}
		if want.json != got.json {
			differing = append(differing, key)
		}
	}
	slices.Sort(differing)

	if len(missing) == 0 && len(extra) == 0 && len(differing) == 0 {
		return "", true
	}

	var report strings.Builder
	fmt.Fprintf(&report, "\nhelm-drift: %s\n", overlay)
	for _, key := range missing {
		fmt.Fprintf(&report, "  the chart does not render %s, which %s has\n", key, overlay)
	}
	for _, key := range extra {
		fmt.Fprintf(&report, "  the chart renders %s, which %s does not have\n", key, overlay)
	}

	crdDiffers := false
	for _, key := range missing {
		if strings.Contains(key, "/CustomResourceDefinition/") {
			crdDiffers = true
		}
	}
	for _, key := range differing {
		fmt.Fprintf(&report, "  %s differs (%s, then the chart):\n", key, overlay)
		fmt.Fprint(&report, diff(deployed[key].document, rendered[key].document))
		if strings.Contains(key, "/CustomResourceDefinition/") {
			crdDiffers = true
		}
	}
	if crdDiffers {
		fmt.Fprintln(&report, "  re-copy the generated CRDs into the chart with 'make helm-crds'")
	}
	return report.String(), false
}

// diff prints a unified diff of two rendered objects. It shells out to `diff`
// so the output is the one every reader already knows: the line that changed,
// with the lines around it.
func diff(deployed, rendered string) string {
	deployedPath, err := writeTemp(deployed)
	if err != nil {
		return fmt.Sprintf("    %v\n", err)
	}
	defer func() { _ = os.Remove(deployedPath) }()

	renderedPath, err := writeTemp(rendered)
	if err != nil {
		return fmt.Sprintf("    %v\n", err)
	}
	defer func() { _ = os.Remove(renderedPath) }()

	//nolint:gosec // the binary is `diff`, and the arguments are temporary file names the guard just created.
	command := exec.Command("diff", "-u",
		"--label", "config/", "--label", "chart",
		deployedPath, renderedPath)
	output, _ := command.Output()

	var indented strings.Builder
	for line := range strings.SplitSeq(strings.TrimSuffix(string(output), "\n"), "\n") {
		fmt.Fprintf(&indented, "    %s\n", line)
	}
	return indented.String()
}

func writeTemp(content string) (string, error) {
	file, err := os.CreateTemp("", "helm-drift-*.json")
	if err != nil {
		return "", fmt.Errorf("creating a temporary file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("writing %s: %w", file.Name(), err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", file.Name(), err)
	}
	return file.Name(), nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "helm-drift: %v\n", err)
	os.Exit(1)
}
