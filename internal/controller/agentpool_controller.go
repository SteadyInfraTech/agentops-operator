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
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
	opmetrics "github.com/SteadyInfraTech/agentops-operator/internal/metrics"
)

// AgentPoolReconciler aggregates member AgentServices and their sessions into
// AgentPool.status (capacity, load, readiness).
type AgentPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentsessions,verbs=get;list;watch

func (r *AgentPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pool := &agentsv1alpha1.AgentPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		if client.IgnoreNotFound(err) == nil {
			opmetrics.DeleteAgentPool(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := pool.DeepCopy()

	sel, err := metav1.LabelSelectorAsSelector(&pool.Spec.Selector)
	if err != nil {
		setPoolCond(pool, metav1.ConditionFalse, "InvalidSelector", err.Error())
		return ctrl.Result{}, r.patchStatus(ctx, orig, pool)
	}

	var svcs agentsv1alpha1.AgentServiceList
	if err := r.List(ctx, &svcs, client.InNamespace(pool.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	var sessions agentsv1alpha1.AgentSessionList
	if err := r.List(ctx, &sessions, client.InNamespace(pool.Namespace),
		client.MatchingFields{indexSessionPoolRef: pool.Name}); err != nil {
		return ctrl.Result{}, err
	}
	active := activeSessionsByAgent(sessions.Items)

	st := agentsv1alpha1.AgentPoolStatus{ObservedGeneration: pool.Generation, Conditions: pool.Status.Conditions}
	sort.Slice(svcs.Items, func(i, j int) bool { return svcs.Items[i].Name < svcs.Items[j].Name })
	for _, s := range svcs.Items {
		st.Agents++
		if s.Status.ReadyReplicas > 0 {
			st.ReadyAgents++
		}
		st.Capacity += s.Status.ReadyReplicas * pool.Spec.MaxSessionsPerReplica
		st.ActiveSessions += active[s.Name]
		st.Members = append(st.Members, agentsv1alpha1.AgentPoolMember{
			Name: s.Name, ReadyReplicas: s.Status.ReadyReplicas, ActiveSessions: active[s.Name],
		})
	}
	pool.Status = st

	switch {
	case st.Agents == 0:
		setPoolCond(pool, metav1.ConditionFalse, "NoMembers", "selector matches no AgentServices")
	case st.ReadyAgents == 0:
		setPoolCond(pool, metav1.ConditionFalse, "NoReadyMembers", "no member AgentService has ready replicas")
	default:
		setPoolCond(pool, metav1.ConditionTrue, "PoolReady",
			fmt.Sprintf("%d/%d agents ready, %d/%d sessions", st.ReadyAgents, st.Agents, st.ActiveSessions, st.Capacity))
	}

	opmetrics.AgentPoolSessions.WithLabelValues(pool.Namespace, pool.Name, "active").Set(float64(st.ActiveSessions))
	opmetrics.AgentPoolSessions.WithLabelValues(pool.Namespace, pool.Name, "capacity").Set(float64(st.Capacity))
	return ctrl.Result{}, r.patchStatus(ctx, orig, pool)
}

func (r *AgentPoolReconciler) patchStatus(ctx context.Context, orig, pool *agentsv1alpha1.AgentPool) error {
	if equality.Semantic.DeepEqual(orig.Status, pool.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, pool, client.MergeFrom(orig)))
}

func setPoolCond(p *agentsv1alpha1.AgentPool, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: agentsv1alpha1.ConditionReady, Status: s, Reason: reason, Message: msg, ObservedGeneration: p.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentPool{}).
		// Membership is label-based, so any AgentService change may affect any
		// pool in its namespace. Pools per namespace are expected to be few.
		Watches(&agentsv1alpha1.AgentService{}, handler.EnqueueRequestsFromMapFunc(r.poolsInNamespace)).
		Watches(&agentsv1alpha1.AgentSession{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				ref := o.(*agentsv1alpha1.AgentSession).Spec.PoolRef
				if ref == nil {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: ref.Name}}}
			})).
		Named("agentpool").
		Complete(r)
}

func (r *AgentPoolReconciler) poolsInNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	var pools agentsv1alpha1.AgentPoolList
	if err := r.List(ctx, &pools, client.InNamespace(o.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "listing AgentPools")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for _, p := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
	}
	return reqs
}
