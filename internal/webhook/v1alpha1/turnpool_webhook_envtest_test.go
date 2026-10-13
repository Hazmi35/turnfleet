// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
	"github.com/hzmi/turnfleet/internal/relay"
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
