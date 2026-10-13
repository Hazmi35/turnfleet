// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package v1alpha1

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

// log is for logging in this package.
var turnpoollog = logf.Log.WithName("turnpool-resource")

// SetupTURNPoolWebhookWithManager registers the webhook for TURNPool in the manager.
func SetupTURNPoolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &turnfleetv1alpha1.TURNPool{}).
		WithValidator(&TURNPoolValidator{}).
		WithDefaulter(&TURNPoolDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-turnfleet-hzmi-dev-v1alpha1-turnpool,mutating=true,failurePolicy=fail,sideEffects=None,groups=turnfleet.hzmi.dev,resources=turnpools,verbs=create;update,versions=v1alpha1,name=mturnpool-v1alpha1.kb.io,admissionReviewVersions=v1

// TURNPoolDefaulter struct is responsible for setting default values on the custom resource of the
// Kind TURNPool when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type TURNPoolDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind TURNPool.
func (d *TURNPoolDefaulter) Default(_ context.Context, obj *turnfleetv1alpha1.TURNPool) error {
	turnpoollog.Info("Defaulting for TURNPool", "name", obj.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-turnfleet-hzmi-dev-v1alpha1-turnpool,mutating=false,failurePolicy=fail,sideEffects=None,groups=turnfleet.hzmi.dev,resources=turnpools,verbs=create;update,versions=v1alpha1,name=vturnpool-v1alpha1.kb.io,admissionReviewVersions=v1

// TURNPoolValidator struct is responsible for validating the TURNPool resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type TURNPoolValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type TURNPool.
func (v *TURNPoolValidator) ValidateCreate(_ context.Context, obj *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	turnpoollog.Info("Validation for TURNPool upon creation", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type TURNPool.
func (v *TURNPoolValidator) ValidateUpdate(_ context.Context, oldObj, newObj *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	turnpoollog.Info("Validation for TURNPool upon update", "name", newObj.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type TURNPool.
func (v *TURNPoolValidator) ValidateDelete(_ context.Context, obj *turnfleetv1alpha1.TURNPool) (admission.Warnings, error) {
	turnpoollog.Info("Validation for TURNPool upon deletion", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
