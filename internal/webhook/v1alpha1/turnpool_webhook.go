// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

// Package v1alpha1 contains the admission webhooks for the TURNPool resource.
//
// Defaulting fills in everything the controller and the renderer need to be
// deterministic; validation rejects the states that would silently destroy
// relay capacity, above all overlapping relay port ranges on one address and
// changes that would invalidate live allocations.
package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
	"github.com/hzmi/turnfleet/internal/relay"
)

// digestPinnedImage matches `repo/name:tag@sha256:<64 hex>` — a tag is optional,
// the digest is not.
var digestPinnedImage = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// log is for logging in this package.
var turnpoollog = logf.Log.WithName("turnpool-webhook")

// groupKind identifies the resource in admission errors.
var groupKind = schema.GroupKind{Group: turnfleetv1alpha1.GroupVersion.Group, Kind: "TURNPool"}

// SetupTURNPoolWebhookWithManager registers the webhook for TURNPool in the manager.
func SetupTURNPoolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &turnfleetv1alpha1.TURNPool{}).
		WithValidator(&TURNPoolValidator{}).
		WithDefaulter(&TURNPoolDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-turnfleet-hzmi-dev-v1alpha1-turnpool,mutating=true,failurePolicy=fail,sideEffects=None,groups=turnfleet.hzmi.dev,resources=turnpools,verbs=create;update,versions=v1alpha1,name=mturnpool-v1alpha1.kb.io,admissionReviewVersions=v1

// TURNPoolDefaulter sets the defaults a pool needs to be reconcilable without
// the controller having to guess.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen
// from generating DeepCopy methods for this type.
//
// +kubebuilder:object:generate=false
type TURNPoolDefaulter struct{}

// Default implements admission.Defaulter.
func (d *TURNPoolDefaulter) Default(_ context.Context, obj *turnfleetv1alpha1.TURNPool) error {
	turnpoollog.V(1).Info("defaulting TURNPool", "name", obj.GetName(), "namespace", obj.GetNamespace())

	if obj.Spec.PeerPolicy == nil {
		obj.Spec.PeerPolicy = &turnfleetv1alpha1.PeerPolicySpec{}
	}
	// The deny list is materialised into the spec rather than kept in the
	// controller so that what a relay will actually be allowed to reach is
	// visible in the object, and so an operator can edit it.
	if len(obj.Spec.PeerPolicy.DeniedCIDRs) == 0 {
		obj.Spec.PeerPolicy.DeniedCIDRs = relay.DefaultDeniedCIDRs()
	}

	if obj.Spec.DrainTimeout == nil {
		obj.Spec.DrainTimeout = &metav1.Duration{Duration: relay.DefaultDrainTimeout}
	}
	if obj.Spec.Probe == nil {
		obj.Spec.Probe = &turnfleetv1alpha1.ProbeSpec{}
	}
	if obj.Spec.Probe.Interval == nil {
		obj.Spec.Probe.Interval = &metav1.Duration{Duration: relay.DefaultProbeInterval}
	}
	if obj.Spec.Probe.Timeout == nil {
		obj.Spec.Probe.Timeout = &metav1.Duration{Duration: relay.DefaultProbeTimeout}
	}

	// The CRD default covers the field, but a defaulting webhook also runs for
	// objects created from an older schema or through a path that skipped
	// defaulting, so keep the two in step.
	if obj.Spec.Addressing.PartitionedIP != nil && obj.Spec.Addressing.PartitionedIP.RelaysPerNode == 0 {
		obj.Spec.Addressing.PartitionedIP.RelaysPerNode = relay.DefaultRelaysPerNode
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-turnfleet-hzmi-dev-v1alpha1-turnpool,mutating=false,failurePolicy=fail,sideEffects=None,groups=turnfleet.hzmi.dev,resources=turnpools,verbs=create;update,versions=v1alpha1,name=vturnpool-v1alpha1.kb.io,admissionReviewVersions=v1

// TURNPoolValidator rejects pools that cannot be relayed safely.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen
// from generating DeepCopy methods for this type.
//
// +kubebuilder:object:generate=false
type TURNPoolValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *TURNPoolValidator) ValidateCreate(_ context.Context, obj *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	turnpoollog.V(1).Info("validating TURNPool creation", "name", obj.GetName(), "namespace", obj.GetNamespace())

	errs := validateSpec(&obj.Spec, field.NewPath("spec"))
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(groupKind, obj.GetName(), errs)
	}
	return nil, nil
}

// ValidateUpdate implements admission.Validator.
func (v *TURNPoolValidator) ValidateUpdate(_ context.Context, oldObj, newObj *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	turnpoollog.V(1).Info("validating TURNPool update", "name", newObj.GetName(), "namespace", newObj.GetNamespace())

	errs := validateSpec(&newObj.Spec, field.NewPath("spec"))
	errs = append(errs, validateImmutability(&oldObj.Spec, &newObj.Spec, field.NewPath("spec"))...)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(groupKind, newObj.GetName(), errs)
	}
	return nil, nil
}

// ValidateDelete implements admission.Validator. Deletion is guarded by the
// drain finalizer, not by admission.
func (v *TURNPoolValidator) ValidateDelete(_ context.Context, _ *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	return nil, nil
}

// validateSpec checks a spec in full; it is used for both create and update so
// that an update can never introduce a state a create would have rejected.
func validateSpec(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	errs := make(field.ErrorList, 0, 8)

	errs = append(errs, validatePortRange(&spec.RelayPortRange, spec.Addressing, path.Child("relayPortRange"))...)
	errs = append(errs, validateListeners(spec, path.Child("listeners"))...)
	errs = append(errs, validateAddressing(spec, path.Child("addressing"))...)
	errs = append(errs, validateDataplane(spec, path.Child("dataplane"))...)
	errs = append(errs, validateAuth(spec, path.Child("auth"))...)
	errs = append(errs, validatePeerPolicy(spec, path.Child("peerPolicy"))...)
	errs = append(errs, validateTimings(spec, path)...)

	return errs
}

// validatePortRange enforces the ordering and size rules the CRD schema cannot
// express, and the one rule that decides whether a partitioned pool can exist at
// all: every relay on a shared address must get at least one port.
func validatePortRange(r *turnfleetv1alpha1.PortRange, addressing turnfleetv1alpha1.AddressingSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if r.Start == 0 && r.End == 0 {
		return append(errs, field.Required(path, "a relay port range is required: capacity is the number of ports in it"))
	}
	if r.End < r.Start {
		errs = append(errs, field.Invalid(path.Child("end"), r.End,
			fmt.Sprintf("must be greater than or equal to start (%d)", r.Start)))
		return errs
	}
	ports := int64(r.End) - int64(r.Start) + 1

	if addressing.Mode == turnfleetv1alpha1.AddressingModePartitionedIP && addressing.PartitionedIP != nil {
		relays := int64(addressing.PartitionedIP.RelaysPerNode)
		if relays < 1 {
			errs = append(errs, field.Invalid(path.Child("start"), r.Start,
				"addressing.partitionedIP.relaysPerNode must be at least 1"))
			return errs
		}
		if ports < relays {
			errs = append(errs, field.Invalid(path, fmt.Sprintf("%d-%d", r.Start, r.End),
				fmt.Sprintf("holds %d ports, fewer than the %d relays per node that would have to share them: "+
					"every relay needs at least one port", ports, relays)))
		}
	}
	return errs
}

// validateListeners enforces coturn's listener model: one listener per protocol,
// UTF and TCP sharing the plain port, TLS carrying a certificate, and no
// listener colliding with the relay port range.
func validateListeners(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	seenProtocol := map[turnfleetv1alpha1.ListenerProtocol]int{}
	var plainPort int32
	plainPortSet := false

	for i, listener := range spec.Listeners {
		item := path.Index(i)

		if prev, dup := seenProtocol[listener.Protocol]; dup {
			errs = append(errs, field.Duplicate(item.Child("protocol"),
				fmt.Sprintf("%s; coturn serves one listener per protocol (listeners[%d] already uses it)", listener.Protocol, prev)))
		} else {
			seenProtocol[listener.Protocol] = i
		}

		if listener.Protocol == turnfleetv1alpha1.ListenerProtocolTLS {
			if listener.TLSSecretRef == nil || listener.TLSSecretRef.Name == "" {
				errs = append(errs, field.Required(item.Child("tlsSecretRef"),
					"a tls listener needs the Secret holding its certificate (tls.crt and tls.key)"))
			}
			continue
		}

		if listener.TLSSecretRef != nil {
			errs = append(errs, field.Forbidden(item.Child("tlsSecretRef"),
				"only a tls listener takes a certificate"))
		}
		// coturn has a single "plain" port: UDP and TCP listeners share it, so
		// two plain listeners on different ports would silently leave one of
		// them unbound.
		switch {
		case !plainPortSet:
			plainPort, plainPortSet = listener.Port, true
		case listener.Port != plainPort:
			errs = append(errs, field.Invalid(item.Child("port"), listener.Port,
				fmt.Sprintf("coturn serves udp and tcp on the same plain port (%d), so they must share it", plainPort)))
		}

		// Every port this listener will be served on has to stay clear of the
		// relay port range: a listener and an allocation cannot share a port.
		// In PartitionedIP mode the pool's relays serve a span of ports
		// starting at the declared one, because each relay on a shared address
		// needs a listening port of its own.
		ports := spec.Addressing.PartitionedIP
		span := int32(1)
		if ports != nil && spec.Addressing.Mode == turnfleetv1alpha1.AddressingModePartitionedIP {
			span = max(ports.RelaysPerNode, 1)
		}
		if listener.Port+span-1 > 65535 {
			errs = append(errs, field.Invalid(item.Child("port"), listener.Port,
				fmt.Sprintf("leaves no room for %d relays on one address: the last relay would need port %d",
					span, listener.Port+span-1)))
		}
		if spec.RelayPortRange.Start != 0 || spec.RelayPortRange.End != 0 {
			for offset := int32(0); offset < span; offset++ {
				port := listener.Port + offset
				if port >= spec.RelayPortRange.Start && port <= spec.RelayPortRange.End {
					errs = append(errs, field.Invalid(item.Child("port"), listener.Port,
						fmt.Sprintf("uses port %d, which falls inside relayPortRange %d-%d: a listener and an allocation cannot share a port",
							port, spec.RelayPortRange.Start, spec.RelayPortRange.End)))
					break
				}
			}
		}
	}

	return errs
}

// validateAddressing enforces that the mode-specific configuration is present
// for the selected mode, absent for the others, and consistent with the replica
// count where that is knowable from the spec alone.
func validateAddressing(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	addressing := spec.Addressing
	switch addressing.Mode {
	case turnfleetv1alpha1.AddressingModeHostNetwork:
		if addressing.ExternalIPPool != nil {
			errs = append(errs, field.Forbidden(path.Child("externalIPPool"),
				"only applies to mode ExternalIPPool"))
		}
		if addressing.PartitionedIP != nil {
			errs = append(errs, field.Forbidden(path.Child("partitionedIP"),
				"only applies to mode PartitionedIP"))
		}

	case turnfleetv1alpha1.AddressingModeExternalIPPool:
		if addressing.PartitionedIP != nil {
			errs = append(errs, field.Forbidden(path.Child("partitionedIP"),
				"only applies to mode PartitionedIP"))
		}
		if addressing.ExternalIPPool == nil || len(addressing.ExternalIPPool.Addresses) == 0 {
			errs = append(errs, field.Required(path.Child("externalIPPool", "addresses"),
				"mode ExternalIPPool needs at least one secondary address to place a relay on"))
			break
		}
		if int64(spec.Replicas) > int64(len(addressing.ExternalIPPool.Addresses)) {
			errs = append(errs, field.Invalid(path.Child("externalIPPool", "addresses"),
				len(addressing.ExternalIPPool.Addresses),
				fmt.Sprintf("only %d addresses are declared, so replicas cannot be %d: one relay per address",
					len(addressing.ExternalIPPool.Addresses), spec.Replicas)))
		}
		for i, addr := range addressing.ExternalIPPool.Addresses {
			if _, err := netip.ParseAddr(strings.TrimSpace(addr.Address)); err != nil {
				errs = append(errs, field.Invalid(path.Child("externalIPPool", "addresses").Index(i).Child("address"),
					addr.Address, "must be an IP address"))
			}
		}

	case turnfleetv1alpha1.AddressingModePartitionedIP:
		if addressing.ExternalIPPool != nil {
			errs = append(errs, field.Forbidden(path.Child("externalIPPool"),
				"only applies to mode ExternalIPPool"))
		}
		if addressing.PartitionedIP == nil {
			errs = append(errs, field.Required(path.Child("partitionedIP"),
				"mode PartitionedIP needs relaysPerNode: it decides how the port range is split"))
		} else if addressing.PartitionedIP.RelaysPerNode < 1 {
			errs = append(errs, field.Invalid(path.Child("partitionedIP", "relaysPerNode"),
				addressing.PartitionedIP.RelaysPerNode, "must be at least 1"))
		}

	default:
		errs = append(errs, field.NotSupported(path.Child("mode"), addressing.Mode, []string{
			string(turnfleetv1alpha1.AddressingModeHostNetwork),
			string(turnfleetv1alpha1.AddressingModeExternalIPPool),
			string(turnfleetv1alpha1.AddressingModePartitionedIP),
		}))
	}

	return errs
}

// validateDataplane requires a digest-pinned relay image: a floating tag would
// change what every relay runs without any object recording it.
func validateDataplane(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	image := strings.TrimSpace(spec.Dataplane.Image)
	if image == "" {
		errs = append(errs, field.Required(path.Child("image"), "the relay image is required"))
		return errs
	}
	if !digestPinnedImage.MatchString(image) {
		errs = append(errs, field.Invalid(path.Child("image"), image,
			"must be pinned by digest (repo/name:tag@sha256:<64 hex>): a relay must never float to a new version"))
	}
	return errs
}

func validateAuth(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if strings.TrimSpace(spec.Auth.Realm) == "" {
		errs = append(errs, field.Required(path.Child("realm"), "a realm is required for long-term credentials"))
	}
	if spec.Auth.SecretRef == nil || spec.Auth.SecretRef.Name == "" {
		errs = append(errs, field.Required(path.Child("secretRef", "name"),
			"the shared HMAC secret is required: it is the same secret the credential issuer uses"))
	} else if spec.Auth.SecretRef.Key == "" {
		errs = append(errs, field.Required(path.Child("secretRef", "key"), "a secret key is required"))
	}
	return errs
}

// validatePeerPolicy rejects unparseable CIDRs. coturn itself takes IP ranges,
// not prefixes, so a typo here would otherwise become a relay that silently
// denies nothing.
func validatePeerPolicy(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if spec.PeerPolicy == nil {
		return errs
	}
	for i, cidr := range spec.PeerPolicy.DeniedCIDRs {
		if err := peerRangeErr(cidr); err != nil {
			errs = append(errs, field.Invalid(path.Child("deniedCIDRs").Index(i), cidr, err.Error()))
		}
	}
	for i, cidr := range spec.PeerPolicy.AllowedCIDRs {
		if err := peerRangeErr(cidr); err != nil {
			errs = append(errs, field.Invalid(path.Child("allowedCIDRs").Index(i), cidr, err.Error()))
		}
	}
	return errs
}

// peerRangeErr reports whether a CIDR can be rendered as a coturn peer range.
func peerRangeErr(cidr string) error {
	if strings.TrimSpace(cidr) == "" {
		return errors.New("empty CIDR")
	}
	if _, err := relay.PeerRange(cidr); err != nil {
		return fmt.Errorf("must be a CIDR such as 10.0.0.0/8: %w", err)
	}
	return nil
}

func validateTimings(spec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if spec.DrainTimeout != nil && spec.DrainTimeout.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("drainTimeout"), spec.DrainTimeout.Duration.String(),
			"must be positive: it is how long a draining relay may keep its allocations"))
	}
	if spec.Probe == nil {
		return errs
	}
	if spec.Probe.Interval != nil && spec.Probe.Interval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("probe", "interval"), spec.Probe.Interval.Duration.String(),
			"must be positive"))
	}
	if spec.Probe.Timeout != nil && spec.Probe.Timeout.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("probe", "timeout"), spec.Probe.Timeout.Duration.String(),
			"must be positive"))
	}
	if spec.Probe.Interval != nil && spec.Probe.Timeout != nil &&
		spec.Probe.Timeout.Duration > spec.Probe.Interval.Duration {
		errs = append(errs, field.Invalid(path.Child("probe", "timeout"), spec.Probe.Timeout.Duration.String(),
			fmt.Sprintf("must not exceed the probe interval (%s): a probe would still be running when the next one starts",
				spec.Probe.Interval.Duration)))
	}
	return errs
}

// validateImmutability rejects changes that would invalidate live allocations.
//
// A relay's identity is its address plus its port range, and every allocation a
// client holds is bound to both. Changing any of these in place would silently
// strand allocations that signalling already handed out, so the only safe move
// is a new TURNPool. Addresses may be added or removed (that drains a relay
// deliberately); the port space and the realm may not move.
func validateImmutability(oldSpec, newSpec *turnfleetv1alpha1.TURNPoolSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if oldSpec.Addressing.Mode != newSpec.Addressing.Mode {
		errs = append(errs, field.Forbidden(path.Child("addressing", "mode"),
			fmt.Sprintf("is immutable (%s -> %s): every relay's advertised address would change and live allocations would be stranded",
				oldSpec.Addressing.Mode, newSpec.Addressing.Mode)))
	}
	if oldSpec.RelayPortRange != newSpec.RelayPortRange {
		errs = append(errs, field.Forbidden(path.Child("relayPortRange"),
			fmt.Sprintf("is immutable (%d-%d -> %d-%d): every allocation is bound to a port in it",
				oldSpec.RelayPortRange.Start, oldSpec.RelayPortRange.End,
				newSpec.RelayPortRange.Start, newSpec.RelayPortRange.End)))
	}
	if oldSpec.Auth.Realm != newSpec.Auth.Realm {
		errs = append(errs, field.Forbidden(path.Child("auth", "realm"),
			fmt.Sprintf("is immutable (%q -> %q): every credential the issuer signed for this pool would be rejected",
				oldSpec.Auth.Realm, newSpec.Auth.Realm)))
	}
	if relaysPerNode(oldSpec) != relaysPerNode(newSpec) {
		errs = append(errs, field.Forbidden(path.Child("addressing", "partitionedIP", "relaysPerNode"),
			"is immutable: it decides how the port range is split, so changing it re-partitions every relay"))
	}
	return errs
}

func relaysPerNode(spec *turnfleetv1alpha1.TURNPoolSpec) int32 {
	if spec.Addressing.PartitionedIP == nil {
		return 0
	}
	return spec.Addressing.PartitionedIP.RelaysPerNode
}
