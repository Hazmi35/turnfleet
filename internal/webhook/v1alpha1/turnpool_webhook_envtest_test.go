// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/Hazmi35/turnfleet/api/v1alpha1"
	"github.com/Hazmi35/turnfleet/internal/relay"
)

// These specs go through the API server, so they prove the webhook is actually
// registered and wired, not just that the validation function returns an error.
var _ = Describe("TURNPool admission through the API server", func() {
	const namespace = "default"

	newPool := func(name string) *turnfleetv1alpha1.TURNPool {
		pool := validPool()
		pool.Name = name
		pool.Namespace = namespace
		return pool
	}

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: namespace, Name: name}
	}

	Context("when a pool is valid", func() {
		const name = "admission-valid"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newPool(name))).To(Succeed())
		})

		AfterEach(func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		})

		It("stores the defaults the webhook filled in", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			Expect(pool.Spec.PeerPolicy).NotTo(BeNil(),
				"the deny list must be materialised so operators can see what a relay may reach")
			Expect(pool.Spec.PeerPolicy.DeniedCIDRs).To(Equal(relay.DefaultDeniedCIDRs()))
			Expect(pool.Spec.DrainTimeout).NotTo(BeNil())
			Expect(pool.Spec.Probe).NotTo(BeNil())
			Expect(pool.Spec.Probe.Interval).NotTo(BeNil())
			Expect(pool.Spec.Probe.Timeout).NotTo(BeNil())
		})

		It("exposes the scale subresource on spec.replicas", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			// Read the subresource as unstructured so the test does not depend
			// on the autoscaling types being in the scheme.
			scale := &unstructured.Unstructured{}
			scale.SetGroupVersionKind(autoscalingv1.SchemeGroupVersion.WithKind("Scale"))
			Expect(k8sClient.SubResource("scale").Get(ctx, pool, scale)).To(Succeed())

			specReplicas, found, err := unstructured.NestedInt64(scale.Object, "spec", "replicas")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "the scale subresource must report spec.replicas")
			Expect(specReplicas).To(Equal(int64(2)))

			// The status replica path is written by the controller; before it
			// has run the API server reports zero rather than failing, which is
			// what keeps the subresource usable by an HPA from the start.
			statusReplicas, _, err := unstructured.NestedInt64(scale.Object, "status", "replicas")
			Expect(err).NotTo(HaveOccurred())
			Expect(statusReplicas).To(Equal(int64(0)))
		})
	})

	Context("when a pool is invalid", func() {
		It("rejects a listener inside the relay port range", func() {
			pool := newPool("admission-bad-listener")
			pool.Spec.Listeners[0].Port = 49160

			err := k8sClient.Create(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("relayPortRange"))
		})

		It("rejects an image that is not digest-pinned", func() {
			pool := newPool("admission-bad-image")
			pool.Spec.Dataplane.Image = "coturn/coturn:latest"

			err := k8sClient.Create(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("sha256"))
		})

		It("rejects a partitioned pool whose range cannot be split", func() {
			pool := newPool("admission-bad-partition")
			pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
				Mode:          turnfleetv1alpha1.AddressingModePartitionedIP,
				PartitionedIP: &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 8},
			}
			pool.Spec.RelayPortRange = turnfleetv1alpha1.PortRange{Start: 49152, End: 49154}

			err := k8sClient.Create(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("every relay needs at least one port"))
		})

		It("makes the mode-specific configuration mutually exclusive", func() {
			pool := newPool("admission-bad-mode-config")
			pool.Spec.Addressing.PartitionedIP = &turnfleetv1alpha1.PartitionedIPSpec{RelaysPerNode: 2}

			err := k8sClient.Create(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("partitionedIP"))
		})
	})

	Context("when a PodIP pool is validated against the cluster", func() {
		newPodIPPool := func(name, attachment string) *turnfleetv1alpha1.TURNPool {
			pool := newPool(name)
			// An empty attachment is the block's zero value: no attachment, and
			// the relay binds the pod's own address on the default network.
			pool.Spec.Addressing = turnfleetv1alpha1.AddressingSpec{
				Mode:  turnfleetv1alpha1.AddressingModePodIP,
				PodIP: turnfleetv1alpha1.PodIPSpec{AttachmentRef: attachment},
			}
			return pool
		}

		// createAttachment creates the NetworkAttachmentDefinition a pool names,
		// the way a cluster that runs Multus has one.
		createAttachment := func(name string) {
			attachment := &unstructured.Unstructured{}
			attachment.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition",
			})
			attachment.SetName(name)
			attachment.SetNamespace(namespace)
			Expect(k8sClient.Create(ctx, attachment)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), attachment)
			})
		}

		// createDeclaringNode creates a relay node that declares the addresses a
		// PodIP pool plans on, and removes it again before the next spec so one
		// spec's bad declaration cannot reject another's pool.
		createDeclaringNode := func(name, declared string) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      map[string]string{"turnfleet.hzmi.dev/relay-node": "true"},
				Annotations: map[string]string{relay.PodAddressesAnnotation: declared},
			}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				// One spec's bad declaration must not reject another's pool, so
				// the node has to be gone before the next spec runs. The timeout
				// is explicit: Gomega's default is one second.
				_ = k8sClient.Delete(context.Background(), node)
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(context.Background(),
						types.NamespacedName{Name: name}, &corev1.Node{}))
				}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())
			})
		}

		It("admits a pool whose declared attachment exists", func() {
			createAttachment("podip-relay-pods")
			pool := newPodIPPool("admission-podip-attached", "podip-relay-pods")
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), pool)
			})
		})

		It("admits a pool without an attachment", func() {
			// The relay binds the pod's own address on the default network; the
			// addresses themselves are declared on the relay nodes, and a node
			// may be labelled and annotated after the pool is admitted.
			pool := newPodIPPool("admission-podip-default-net", "")
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), pool)
			})
		})

		It("rejects podIP without an attachmentRef", func() {
			// The block is a spelling of "use an attachment" only when it
			// names one: its one field must be set whenever the block is, and
			// the reference must be non-empty, so `podIP: {}` is refused by the
			// schema — through admission as well, where the defaulting patch
			// leaves the refusal to the schema rather than filling the field
			// in.
			pool := newPodIPPool("admission-podip-empty-block", "")
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pool)
			Expect(err).NotTo(HaveOccurred())
			object["apiVersion"] = turnfleetv1alpha1.GroupVersion.String()
			object["kind"] = "TURNPool"
			// ToUnstructured writes the zero block as an absent one; set the
			// literal `podIP: {}` a pool must not be able to send.
			Expect(unstructured.SetNestedField(object, map[string]any{}, "spec", "addressing", "podIP")).To(Succeed())

			err = k8sClient.Create(ctx, &unstructured.Unstructured{Object: object})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("at least one of the fields in [attachmentRef] must be set"))
		})

		It("rejects a declared attachment that does not exist", func() {
			err := k8sClient.Create(ctx, newPodIPPool("admission-podip-missing", "podip-not-created"))
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("does not exist"))
		})

		It("admits a pool whose nodes declare routable addresses", func() {
			createDeclaringNode("podip-node-routable", "203.0.113.21/24,203.0.113.22/24")
			pool := newPodIPPool("admission-podip-routable", "")
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), pool)
			})
		})

		It("admits a pool whose nodes declare private addresses", func() {
			// Admission refuses only what cannot work at all. A private,
			// carrier-grade NAT or IPv6 unique local address is the operator's
			// judgement — a hub-and-spoke cluster whose pod CIDR is peered
			// while the node subnet is not routes it — and the external
			// vantage point (ADR 7) is what proves reachability.
			createDeclaringNode("podip-node-private", "10.0.0.5/24,100.64.0.5/10,fd00::5/64")
			pool := newPodIPPool("admission-podip-private", "")
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), pool)
			})
		})

		for _, rejected := range []struct {
			name     string
			declared string
			reason   string
		}{
			{"a link-local address", "169.254.10.10/16", "link-local"},
			{"a loopback address", "127.0.0.5", "loopback"},
			{"a multicast address", "224.0.0.1", "multicast"},
			{"an unspecified address", "0.0.0.0", "unspecified"},
		} {
			It("rejects a node that declares "+rejected.name, func() {
				createDeclaringNode("podip-node-unusable", rejected.declared)
				err := k8sClient.Create(ctx, newPodIPPool("admission-podip-unusable", ""))
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
				Expect(err.Error()).To(ContainSubstring(rejected.reason))
			})
		}

		It("rejects a malformed declaration", func() {
			createDeclaringNode("podip-node-malformed", "203.0.113.21,rubbish")
			err := k8sClient.Create(ctx, newPodIPPool("admission-podip-malformed", ""))
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("not an IP address"))
		})

		It("keeps the attachment immutable", func() {
			createAttachment("podip-relay-pods")
			createAttachment("podip-relay-pods-v2")
			pool := newPodIPPool("admission-podip-immutable", "podip-relay-pods")
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), pool)
			})

			stored := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key("admission-podip-immutable"), stored)).To(Succeed())
			stored.Spec.Addressing.PodIP.AttachmentRef = "podip-relay-pods-v2"
			err := k8sClient.Update(ctx, stored)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("immutable"))

			// Scaling the same pool stays allowed: the read-free check must not
			// make an ordinary update fail.
			Expect(k8sClient.Get(ctx, key("admission-podip-immutable"), stored)).To(Succeed())
			stored.Spec.Replicas = 5
			Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		})
	})

	Context("when an update would strand live allocations", func() {
		const name = "admission-immutable"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newPool(name))).To(Succeed())
		})

		AfterEach(func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		})

		It("rejects a change to the relay port range", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			pool.Spec.RelayPortRange.End = 49500
			err := k8sClient.Update(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("rejects a change to the realm", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			pool.Spec.Auth.Realm = "elsewhere.example.com"
			err := k8sClient.Update(ctx, pool)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error was %v", err)
			Expect(err.Error()).To(ContainSubstring("realm"))
		})

		It("allows scaling the pool", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			pool.Spec.Replicas = 5
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
		})

		It("allows rotating the shared secret", func() {
			pool := &turnfleetv1alpha1.TURNPool{}
			Expect(k8sClient.Get(ctx, key(name), pool)).To(Succeed())

			pool.Spec.Auth.SecretRef.Name = "turn-secret-rotated"
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
		})
	})
})
