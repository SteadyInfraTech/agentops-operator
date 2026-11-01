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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

const (
	// pendingRequeue is how often unschedulable sessions are retried in addition
	// to event-driven retries triggered by AgentService changes.
	pendingRequeue = 15 * time.Second

	// reasonUnschedulable is the Ready condition reason of a pending session
	// that no target can currently host.
	reasonUnschedulable = "Unschedulable"
)

// AgentSessionReconciler schedules AgentSessions onto AgentServices and
// manages their lifecycle (binding, expiry, completion, TTL cleanup).
type AgentSessionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader bypasses the informer cache. Scheduling reads live session
	// state so that back-to-back placements do not over-commit an agent
	// because the cache has not yet observed the previous binding.
	APIReader client.Reader
	// Now is overridable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentsessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentsessions/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.agentops.io,resources=agentpools,verbs=get;list;watch

func (r *AgentSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sess := &agentsv1alpha1.AgentSession{}
	if err := r.Get(ctx, req.NamespacedName, sess); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sess.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := sess.DeepCopy()
	now := r.now()

	var res ctrl.Result
	var err error
	switch sess.Status.Phase {
	case agentsv1alpha1.AgentSessionCompleted, agentsv1alpha1.AgentSessionExpired, agentsv1alpha1.AgentSessionFailed:
		return r.handleFinished(ctx, sess, now)
	case agentsv1alpha1.AgentSessionBound:
		res, err = r.handleBound(ctx, sess, now)
	default:
		res, err = r.handlePending(ctx, sess, now)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchStatus(ctx, orig, sess); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *AgentSessionReconciler) handlePending(ctx context.Context, sess *agentsv1alpha1.AgentSession, now time.Time) (ctrl.Result, error) {
	if sess.Spec.Completed {
		r.finish(sess, agentsv1alpha1.AgentSessionCompleted, "Completed", "session completed before binding", now)
		return ctrl.Result{}, nil
	}

	target, msg, err := r.schedule(ctx, sess)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target == nil {
		// Count the outcome only when the session first becomes unschedulable;
		// pending sessions are retried every pendingRequeue and would otherwise
		// inflate the counter on each retry.
		if c := meta.FindStatusCondition(sess.Status.Conditions, agentsv1alpha1.ConditionReady); c == nil || c.Reason != reasonUnschedulable {
			opmetrics.RecordSessionAssignment(sess.Namespace, sessionPool(sess), opmetrics.AssignmentUnschedulable)
		}
		r.transition(sess, agentsv1alpha1.AgentSessionPending)
		sess.Status.Message = msg
		setSessCond(sess, metav1.ConditionFalse, reasonUnschedulable, msg)
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}

	r.transition(sess, agentsv1alpha1.AgentSessionBound)
	sess.Status.AssignedAgent = target.Name
	sess.Status.Endpoint = target.Status.Endpoint
	sess.Status.BoundAt = &metav1.Time{Time: now}
	sess.Status.Message = fmt.Sprintf("bound to AgentService %q", target.Name)
	setSessCond(sess, metav1.ConditionTrue, "Bound", sess.Status.Message)
	logf.FromContext(ctx).Info("session bound", "agent", target.Name)
	opmetrics.RecordSessionAssignment(sess.Namespace, sessionPool(sess), opmetrics.AssignmentBound)
	return expiryResult(sess, now), nil
}

// sessionPool returns the pool name used as a metrics label for a session.
func sessionPool(sess *agentsv1alpha1.AgentSession) string {
	if sess.Spec.PoolRef != nil {
		return sess.Spec.PoolRef.Name
	}
	return opmetrics.DirectPool
}

// schedule returns the target AgentService, or nil with a human-readable reason.
func (r *AgentSessionReconciler) schedule(ctx context.Context, sess *agentsv1alpha1.AgentSession) (*agentsv1alpha1.AgentService, string, error) {
	ns := sess.Namespace

	if ref := sess.Spec.AgentServiceRef; ref != nil {
		svc := &agentsv1alpha1.AgentService{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, svc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Sprintf("AgentService %q not found", ref.Name), nil
			}
			return nil, "", err
		}
		if svc.Status.ReadyReplicas == 0 {
			return nil, fmt.Sprintf("AgentService %q has no ready replicas", ref.Name), nil
		}
		return svc, "", nil
	}

	pool := &agentsv1alpha1.AgentPool{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: sess.Spec.PoolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("AgentPool %q not found", sess.Spec.PoolRef.Name), nil
		}
		return nil, "", err
	}
	sel, err := metav1.LabelSelectorAsSelector(&pool.Spec.Selector)
	if err != nil {
		return nil, fmt.Sprintf("AgentPool %q has an invalid selector: %v", pool.Name, err), nil
	}
	var members agentsv1alpha1.AgentServiceList
	if err := r.List(ctx, &members, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, "", err
	}
	// Live read (see APIReader) of all sessions in the namespace: sessions
	// pinned via agentServiceRef also consume capacity on pool members.
	var sessions agentsv1alpha1.AgentSessionList
	if err := r.APIReader.List(ctx, &sessions, client.InNamespace(ns)); err != nil {
		return nil, "", err
	}
	name := pickAgent(members.Items, activeSessionsByAgent(sessions.Items), pool.Spec.MaxSessionsPerReplica)
	if name == "" {
		return nil, fmt.Sprintf("AgentPool %q has no free capacity", pool.Name), nil
	}
	for i := range members.Items {
		if members.Items[i].Name == name {
			return &members.Items[i], "", nil
		}
	}
	return nil, "", fmt.Errorf("selected agent %q missing from member list", name)
}

func (r *AgentSessionReconciler) handleBound(ctx context.Context, sess *agentsv1alpha1.AgentSession, now time.Time) (ctrl.Result, error) {
	if sess.Spec.Completed {
		r.finish(sess, agentsv1alpha1.AgentSessionCompleted, "Completed", "session completed", now)
		return requeueTTL(sess), nil
	}
	if d := sess.Spec.MaxDurationSeconds; d > 0 && sess.Status.BoundAt != nil &&
		!now.Before(sess.Status.BoundAt.Add(time.Duration(d)*time.Second)) {
		r.finish(sess, agentsv1alpha1.AgentSessionExpired, "Expired", fmt.Sprintf("exceeded maxDurationSeconds=%d", d), now)
		return requeueTTL(sess), nil
	}

	svc := &agentsv1alpha1.AgentService{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Status.AssignedAgent}, svc)
	if apierrors.IsNotFound(err) {
		r.finish(sess, agentsv1alpha1.AgentSessionFailed, "AgentGone",
			fmt.Sprintf("assigned AgentService %q was deleted", sess.Status.AssignedAgent), now)
		return requeueTTL(sess), nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	sess.Status.Endpoint = svc.Status.Endpoint
	return expiryResult(sess, now), nil
}

func (r *AgentSessionReconciler) handleFinished(ctx context.Context, sess *agentsv1alpha1.AgentSession, now time.Time) (ctrl.Result, error) {
	ttl := sess.Spec.TTLSecondsAfterFinished
	if ttl == nil || sess.Status.FinishedAt == nil {
		return ctrl.Result{}, nil
	}
	if remaining := sess.Status.FinishedAt.Add(time.Duration(*ttl) * time.Second).Sub(now); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	logf.FromContext(ctx).Info("deleting finished session after TTL")
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, sess, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

func (r *AgentSessionReconciler) finish(sess *agentsv1alpha1.AgentSession, phase agentsv1alpha1.AgentSessionPhase, reason, msg string, now time.Time) {
	r.transition(sess, phase)
	sess.Status.FinishedAt = &metav1.Time{Time: now}
	sess.Status.Message = msg
	setSessCond(sess, metav1.ConditionFalse, reason, msg)
}

func (r *AgentSessionReconciler) transition(sess *agentsv1alpha1.AgentSession, phase agentsv1alpha1.AgentSessionPhase) {
	if sess.Status.Phase != phase {
		opmetrics.AgentSessionTransitions.WithLabelValues(sess.Namespace, string(phase)).Inc()
	}
	sess.Status.Phase = phase
}

func (r *AgentSessionReconciler) patchStatus(ctx context.Context, orig, sess *agentsv1alpha1.AgentSession) error {
	if equality.Semantic.DeepEqual(orig.Status, sess.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, sess, client.MergeFrom(orig)))
}

func (r *AgentSessionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func expiryResult(sess *agentsv1alpha1.AgentSession, now time.Time) ctrl.Result {
	d := sess.Spec.MaxDurationSeconds
	if d <= 0 || sess.Status.BoundAt == nil {
		return ctrl.Result{}
	}
	remaining := sess.Status.BoundAt.Add(time.Duration(d) * time.Second).Sub(now)
	if remaining <= 0 {
		remaining = time.Second
	}
	return ctrl.Result{RequeueAfter: remaining}
}

func requeueTTL(sess *agentsv1alpha1.AgentSession) ctrl.Result {
	if ttl := sess.Spec.TTLSecondsAfterFinished; ttl != nil {
		return ctrl.Result{RequeueAfter: time.Duration(*ttl)*time.Second + time.Second}
	}
	return ctrl.Result{}
}

func setSessCond(sess *agentsv1alpha1.AgentSession, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type: agentsv1alpha1.ConditionReady, Status: s, Reason: reason, Message: msg, ObservedGeneration: sess.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentSessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentSession{}).
		// Agent readiness changes can unblock pending sessions or fail bound ones.
		Watches(&agentsv1alpha1.AgentService{}, handler.EnqueueRequestsFromMapFunc(r.sessionsForAgent)).
		Named("agentsession").
		Complete(r)
}

// sessionsForAgent enqueues non-terminal sessions in the namespace of a changed AgentService.
func (r *AgentSessionReconciler) sessionsForAgent(ctx context.Context, o client.Object) []reconcile.Request {
	var list agentsv1alpha1.AgentSessionList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "listing AgentSessions")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		switch s.Status.Phase {
		case "", agentsv1alpha1.AgentSessionPending:
		case agentsv1alpha1.AgentSessionBound:
			if s.Status.AssignedAgent != o.GetName() {
				continue
			}
		default:
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return reqs
}
