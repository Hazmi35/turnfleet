// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Turnfleet Authors

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	turnfleetv1alpha1 "github.com/hzmi/turnfleet/api/v1alpha1"
)

// TURNPoolReconciler reconciles a TURNPool object
type TURNPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=turnfleet.hzmi.dev,resources=turnpools/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the TURNPool object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *TURNPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TURNPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&turnfleetv1alpha1.TURNPool{}).
		Named("turnpool").
		Complete(r)
}
