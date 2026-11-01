/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// AgentRuntimeReconciler maintains AgentRuntime status (usage count, readiness).
// Propagating runtime changes to workloads is done by the AgentService
// controller, which watches AgentRuntimes.
type AgentRuntimeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentruntimes/finalizers,verbs=update

func (r *AgentRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rt := &agentsv1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := rt.DeepCopy()

	var svcs agentsv1alpha1.AgentServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(rt.Namespace),
		client.MatchingFields{indexServiceRuntimeRef: rt.Name}); err != nil {
		return ctrl.Result{}, err
	}

	rt.Status.ObservedGeneration = rt.Generation
	rt.Status.AgentServices = int32(len(svcs.Items))
	meta.SetStatusCondition(&rt.Status.Conditions, metav1.Condition{
		Type:               agentsv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            fmt.Sprintf("referenced by %d AgentService(s)", len(svcs.Items)),
		ObservedGeneration: rt.Generation,
	})

	if equality.Semantic.DeepEqual(orig.Status, rt.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, rt, client.MergeFrom(orig)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentRuntime{}).
		Watches(&agentsv1alpha1.AgentService{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				ref := o.(*agentsv1alpha1.AgentService).Spec.RuntimeRef
				if ref == nil {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: ref.Name}}}
			})).
		Named("agentruntime").
		Complete(r)
}
