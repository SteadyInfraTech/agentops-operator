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
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
	"github.com/SteadyInfraTech/agentops-operator/internal/agent"
	opmetrics "github.com/SteadyInfraTech/agentops-operator/internal/metrics"
)

// AgentServiceReconciler reconciles an AgentService into a Deployment, a
// Service and an optional HorizontalPodAutoscaler.
type AgentServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// KEDAAvailable says the keda.sh ScaledObject API exists in the cluster
	// (detected once at startup, see DetectKEDA).
	KEDAAvailable bool
	// PrometheusAddress is the Prometheus KEDA queries for the active-sessions
	// gauge. Without it autoscaling.targetSessionsPerPod cannot be honored.
	PrometheusAddress string
}

// deploymentKind is the kind of the workload an HPA must target to scale an agent.
const deploymentKind = "Deployment"

// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentruntimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.steadyinfra.com,resources=agentsessions,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the generated workload objects towards the AgentService spec
// and reports their observed state back into AgentService.status.
func (r *AgentServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	log := logf.FromContext(ctx)

	svc := &agentsv1alpha1.AgentService{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			// Owned objects are garbage-collected via owner references.
			opmetrics.DeleteAgentService(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !svc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := svc.DeepCopy()

	// Derived from the spec alone, so it is set before anything that can fail,
	// and the deferred patch persists it (and any other status change) on every
	// return. A failed patch never hides the reconcile error that came first.
	meta.SetStatusCondition(&svc.Status.Conditions, agent.DrainSafeCondition(svc))
	defer func() {
		if statusErr := r.patchStatus(ctx, orig, svc); statusErr != nil {
			if reconcileErr == nil {
				reconcileErr = statusErr
			} else {
				reconcileErr = fmt.Errorf("%w; persisting status: %v", reconcileErr, statusErr)
			}
		}
	}()

	// Recorded before any early return so the gauge stays current even while
	// the workload is blocked (e.g. a missing AgentRuntime). A failure is
	// returned so the request is retried instead of leaving a stale value.
	if err := r.recordActiveSessions(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}

	var rt *agentsv1alpha1.AgentRuntime
	if ref := svc.Spec.RuntimeRef; ref != nil {
		rt = &agentsv1alpha1.AgentRuntime{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: ref.Name}, rt); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			// The AgentRuntime watch re-triggers us once the runtime appears.
			msg := fmt.Sprintf("AgentRuntime %q not found", ref.Name)
			r.setBlocked(svc, agentsv1alpha1.AgentServicePending, "RuntimeNotFound", msg)
			return ctrl.Result{}, nil
		}
	}

	resolved := agent.Resolve(svc, rt)
	if resolved.Image == "" {
		r.setBlocked(svc, agentsv1alpha1.AgentServiceFailed, "NoImage", "no image set on AgentService or AgentRuntime")
		return ctrl.Result{}, nil
	}

	dep, err := r.reconcileDeployment(ctx, svc, resolved)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Deployment: %w", err)
	}
	if err := r.reconcileService(ctx, svc, resolved); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Service: %w", err)
	}
	if err := r.reconcileAutoscaler(ctx, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling autoscaler: %w", err)
	}

	r.updateStatus(svc, dep, resolved)
	if orig.Status.Phase != svc.Status.Phase {
		log.Info("phase changed", "from", orig.Status.Phase, "to", svc.Status.Phase)
		r.Recorder.Eventf(svc, corev1.EventTypeNormal, "PhaseChanged", "%s -> %s", orig.Status.Phase, svc.Status.Phase)
	}
	return ctrl.Result{}, nil
}

// recordActiveSessions publishes how many sessions are bound to svc. It counts
// with the same rule the scheduler uses (activeSessionsByAgent). The gauge is a
// scaling signal, so a failed read is returned for the caller to retry rather
// than leaving the previous, possibly stale, value in place.
func (r *AgentServiceReconciler) recordActiveSessions(ctx context.Context, svc *agentsv1alpha1.AgentService) error {
	var sessions agentsv1alpha1.AgentSessionList
	if err := r.List(ctx, &sessions, client.InNamespace(svc.Namespace),
		client.MatchingFields{indexSessionAssignedAgent: svc.Name}); err != nil {
		return fmt.Errorf("listing AgentSessions for the active-sessions metric: %w", err)
	}
	active := activeSessionsByAgent(sessions.Items)[svc.Name]
	opmetrics.AgentServiceActiveSessions.WithLabelValues(svc.Namespace, svc.Name).Set(float64(active))
	return nil
}

func (r *AgentServiceReconciler) reconcileDeployment(ctx context.Context, svc *agentsv1alpha1.AgentService, res agent.Resolved) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		agent.MutateDeployment(dep, svc, res)
		return controllerutil.SetControllerReference(svc, dep, r.Scheme)
	})
	return dep, err
}

func (r *AgentServiceReconciler) reconcileService(ctx context.Context, svc *agentsv1alpha1.AgentService, res agent.Resolved) error {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		agent.MutateService(s, svc, res)
		return controllerutil.SetControllerReference(svc, s, r.Scheme)
	})
	return err
}

// useKEDA reports whether the autoscaler for svc is a KEDA ScaledObject. KEDA
// creates its own HPA for the Deployment, so the operator must never keep an
// HPA of its own next to it.
func (r *AgentServiceReconciler) useKEDA(svc *agentsv1alpha1.AgentService) bool {
	return r.KEDAAvailable && r.PrometheusAddress != "" && agent.SessionScalingRequested(svc)
}

func (r *AgentServiceReconciler) reconcileAutoscaler(ctx context.Context, svc *agentsv1alpha1.AgentService) error {
	if agent.SessionScalingRequested(svc) && !r.useKEDA(svc) {
		r.Recorder.Event(svc, corev1.EventTypeWarning, "SessionScalingUnavailable",
			"autoscaling.targetSessionsPerPod needs KEDA installed and the operator started with "+
				"--prometheus-address; scaling on CPU/memory only")
	}

	if r.useKEDA(svc) {
		// Conflicts are checked before anything is deleted: while one exists the
		// switch cannot complete, and removing our HPA would leave the service
		// with no autoscaler. Their objects are left alone, and the watches on
		// HPAs and ScaledObjects bring us back when the conflict is gone.
		conflict, err := r.kedaConflict(ctx, svc)
		if err != nil {
			return err
		}
		if conflict != "" {
			r.Recorder.Event(svc, corev1.EventTypeWarning, "AutoscalerConflict", conflict)
			return nil
		}
		// KEDA's admission refuses a ScaledObject while another HPA targets the
		// Deployment, so the outgoing HPA is removed first.
		if err := r.reconcileHPA(ctx, svc, false); err != nil {
			return err
		}
		return r.reconcileScaledObject(ctx, svc)
	}
	if r.KEDAAvailable {
		if err := r.deleteOwnedScaledObject(ctx, svc); err != nil {
			return err
		}
		// KEDA's HPA outlives the ScaledObject for a moment (it is garbage
		// collected), and two HPAs on one Deployment stop it from scaling at
		// all. Wait for it to go; the HPA watch re-enqueues us when it does.
		if agent.HPAEnabled(svc) {
			pending, err := r.kedaHPAPending(ctx, svc)
			if err != nil {
				return err
			}
			if pending {
				r.Recorder.Event(svc, corev1.EventTypeNormal, "WaitingForKEDAHPA",
					"waiting for the HorizontalPodAutoscaler KEDA created to be removed before creating ours")
				return nil
			}
		}
	}
	return r.reconcileHPA(ctx, svc, agent.HPAEnabled(svc))
}

// kedaHPAPending reports whether an HPA owned by the ScaledObject named after
// svc still targets its Deployment.
func (r *AgentServiceReconciler) kedaHPAPending(ctx context.Context, svc *agentsv1alpha1.AgentService) (bool, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &list, client.InNamespace(svc.Namespace)); err != nil {
		return false, fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	for i := range list.Items {
		h := &list.Items[i]
		if t := h.Spec.ScaleTargetRef; t.Kind == deploymentKind && t.Name == svc.Name && ownedByServiceScaledObject(h, svc) {
			return true, nil
		}
	}
	return false, nil
}

// ownedByServiceScaledObject reports whether hpa is owned by the KEDA
// ScaledObject named after svc, i.e. the HPA KEDA made for our ScaledObject.
func ownedByServiceScaledObject(hpa *autoscalingv2.HorizontalPodAutoscaler, svc *agentsv1alpha1.AgentService) bool {
	for _, o := range hpa.OwnerReferences {
		if o.Kind == agent.ScaledObjectGVK.Kind && o.Name == svc.Name &&
			strings.HasPrefix(o.APIVersion, agent.ScaledObjectGVK.Group+"/") {
			return true
		}
	}
	return false
}

// foreignHPA returns the name of an HPA, not created by this operator or by
// KEDA, that targets svc's Deployment, or "" when there is none.
func (r *AgentServiceReconciler) foreignHPA(ctx context.Context, svc *agentsv1alpha1.AgentService) (string, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &list, client.InNamespace(svc.Namespace)); err != nil {
		return "", fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	for i := range list.Items {
		if isForeignHPA(&list.Items[i], svc) {
			return list.Items[i].Name, nil
		}
	}
	return "", nil
}

// isForeignHPA reports whether hpa scales svc's Deployment but is not ours: not
// created by the operator, and not the HPA KEDA made for the ScaledObject named
// after svc. An HPA owned by any other ScaledObject is a conflict, because KEDA
// refuses a second ScaledObject on a workload another one already scales.
func isForeignHPA(hpa *autoscalingv2.HorizontalPodAutoscaler, svc *agentsv1alpha1.AgentService) bool {
	t := hpa.Spec.ScaleTargetRef
	if t.Kind != deploymentKind || t.Name != svc.Name || metav1.IsControlledBy(hpa, svc) {
		return false
	}
	return !ownedByServiceScaledObject(hpa, svc)
}

// servicesForScaledObject enqueues the AgentService a ScaledObject is named
// after; the operator names its own ScaledObject after the service.
func servicesForScaledObject(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}}}
}

// servicesForHPA enqueues the AgentService whose Deployment an HPA targets, so
// removing a conflicting HPA lets the ScaledObject be created.
func servicesForHPA(_ context.Context, o client.Object) []reconcile.Request {
	hpa, ok := o.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok || hpa.Spec.ScaleTargetRef.Kind != deploymentKind {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: hpa.Namespace, Name: hpa.Spec.ScaleTargetRef.Name,
	}}}
}

// kedaConflict describes why the switch to a ScaledObject cannot complete, or
// returns "" when nothing is in the way: a same-named ScaledObject this
// operator does not control, or an HPA that is not ours already scaling the
// Deployment.
func (r *AgentServiceReconciler) kedaConflict(ctx context.Context, svc *agentsv1alpha1.AgentService) (string, error) {
	existing := agent.NewScaledObject(svc)
	switch err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", err
	case !metav1.IsControlledBy(existing, svc):
		return fmt.Sprintf("ScaledObject %q exists and is not managed by this operator; leaving it unchanged",
			existing.GetName()), nil
	}
	foreign, err := r.foreignHPA(ctx, svc)
	if err != nil {
		return "", err
	}
	if foreign != "" {
		return fmt.Sprintf("HorizontalPodAutoscaler %q already targets Deployment %q; remove it so KEDA can scale on sessions",
			foreign, svc.Name), nil
	}
	return "", nil
}

// errForeignScaledObject means a ScaledObject the operator does not control
// showed up under our name after kedaConflict looked.
var errForeignScaledObject = errors.New("ScaledObject is not managed by this operator")

func (r *AgentServiceReconciler) reconcileScaledObject(ctx context.Context, svc *agentsv1alpha1.AgentService) error {
	so := agent.NewScaledObject(svc)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, so, func() error {
		// CreateOrUpdate reads again, so this also covers an object created
		// since kedaConflict: never mutate or adopt one that is not ours.
		if so.GetUID() != "" && !metav1.IsControlledBy(so, svc) {
			return errForeignScaledObject
		}
		if err := agent.MutateScaledObject(so, svc, r.PrometheusAddress); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(svc, so, r.Scheme)
	})
	if errors.Is(err, errForeignScaledObject) {
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "AutoscalerConflict",
			"ScaledObject %q exists and is not managed by this operator; leaving it unchanged", so.GetName())
		return nil
	}
	return err
}

// deleteOwnedScaledObject removes the ScaledObject this operator created.
// A user-managed one is never touched.
func (r *AgentServiceReconciler) deleteOwnedScaledObject(ctx context.Context, svc *agentsv1alpha1.AgentService) error {
	so := agent.NewScaledObject(svc)
	if err := r.Get(ctx, client.ObjectKeyFromObject(so), so); err != nil {
		return client.IgnoreNotFound(err)
	}
	if metav1.IsControlledBy(so, svc) {
		return client.IgnoreNotFound(r.Delete(ctx, so))
	}
	return nil
}

// reconcileHPA makes the HPA match want: it creates or updates it when wanted
// and deletes the one this operator owns otherwise.
func (r *AgentServiceReconciler) reconcileHPA(ctx context.Context, svc *agentsv1alpha1.AgentService, want bool) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	if !want {
		// Only delete an HPA we own; never touch a user-managed one.
		if err := r.Get(ctx, client.ObjectKeyFromObject(hpa), hpa); err != nil {
			return client.IgnoreNotFound(err)
		}
		if metav1.IsControlledBy(hpa, svc) {
			return client.IgnoreNotFound(r.Delete(ctx, hpa))
		}
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		agent.MutateHPA(hpa, svc)
		return controllerutil.SetControllerReference(svc, hpa, r.Scheme)
	})
	return err
}

func (r *AgentServiceReconciler) updateStatus(svc *agentsv1alpha1.AgentService, dep *appsv1.Deployment, res agent.Resolved) {
	st := &svc.Status
	st.ObservedGeneration = svc.Generation
	st.Image = res.Image
	st.Endpoint = agent.Endpoint(svc, res.Port)
	st.Replicas = dep.Status.Replicas
	st.ReadyReplicas = dep.Status.ReadyReplicas
	desired := ptr.Deref(dep.Spec.Replicas, 1)

	stalled := false
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			stalled = true
		}
	}
	upToDate := dep.Status.ObservedGeneration >= dep.Generation && dep.Status.UpdatedReplicas >= desired

	switch {
	case stalled:
		st.Phase = agentsv1alpha1.AgentServiceDegraded
		setCond(svc, agentsv1alpha1.ConditionReady, metav1.ConditionFalse, "ProgressDeadlineExceeded", "Deployment rollout stalled")
		setCond(svc, agentsv1alpha1.ConditionDegraded, metav1.ConditionTrue, "ProgressDeadlineExceeded", "Deployment rollout stalled")
		setCond(svc, agentsv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ProgressDeadlineExceeded", "Deployment rollout stalled")
	case upToDate && st.ReadyReplicas >= desired:
		st.Phase = agentsv1alpha1.AgentServiceRunning
		msg := fmt.Sprintf("%d/%d replicas ready", st.ReadyReplicas, desired)
		setCond(svc, agentsv1alpha1.ConditionReady, metav1.ConditionTrue, "AgentAvailable", msg)
		setCond(svc, agentsv1alpha1.ConditionDegraded, metav1.ConditionFalse, "AgentAvailable", msg)
		setCond(svc, agentsv1alpha1.ConditionProgressing, metav1.ConditionFalse, "RolloutComplete", msg)
	default:
		st.Phase = agentsv1alpha1.AgentServiceProgressing
		msg := fmt.Sprintf("%d/%d replicas ready", st.ReadyReplicas, desired)
		setCond(svc, agentsv1alpha1.ConditionReady, metav1.ConditionFalse, "RolloutInProgress", msg)
		setCond(svc, agentsv1alpha1.ConditionDegraded, metav1.ConditionFalse, "RolloutInProgress", msg)
		setCond(svc, agentsv1alpha1.ConditionProgressing, metav1.ConditionTrue, "RolloutInProgress", msg)
	}

	opmetrics.AgentServiceReplicas.WithLabelValues(svc.Namespace, svc.Name, "desired").Set(float64(desired))
	opmetrics.AgentServiceReplicas.WithLabelValues(svc.Namespace, svc.Name, "current").Set(float64(st.Replicas))
	opmetrics.AgentServiceReplicas.WithLabelValues(svc.Namespace, svc.Name, "ready").Set(float64(st.ReadyReplicas))
	recordPhase(svc)
}

func (r *AgentServiceReconciler) setBlocked(svc *agentsv1alpha1.AgentService, phase agentsv1alpha1.AgentServicePhase, reason, msg string) {
	svc.Status.ObservedGeneration = svc.Generation
	svc.Status.Phase = phase
	setCond(svc, agentsv1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
	setCond(svc, agentsv1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, msg)
	r.Recorder.Event(svc, corev1.EventTypeWarning, reason, msg)
	recordPhase(svc)
}

func (r *AgentServiceReconciler) patchStatus(ctx context.Context, orig, svc *agentsv1alpha1.AgentService) error {
	if equality.Semantic.DeepEqual(orig.Status, svc.Status) {
		return nil
	}
	return r.Status().Patch(ctx, svc, client.MergeFrom(orig))
}

func recordPhase(svc *agentsv1alpha1.AgentService) {
	for _, p := range []agentsv1alpha1.AgentServicePhase{
		agentsv1alpha1.AgentServicePending, agentsv1alpha1.AgentServiceProgressing,
		agentsv1alpha1.AgentServiceRunning, agentsv1alpha1.AgentServiceDegraded, agentsv1alpha1.AgentServiceFailed,
	} {
		v := 0.0
		if svc.Status.Phase == p {
			v = 1
		}
		opmetrics.AgentServicePhase.WithLabelValues(svc.Namespace, svc.Name, string(p)).Set(v)
	}
}

func setCond(svc *agentsv1alpha1.AgentService, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: svc.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(&agentsv1alpha1.AgentRuntime{}, handler.EnqueueRequestsFromMapFunc(r.servicesForRuntime)).
		// Binding or finishing a session changes the active-sessions gauge.
		Watches(&agentsv1alpha1.AgentSession{}, handler.EnqueueRequestsFromMapFunc(servicesForSession))
	if r.KEDAAvailable {
		b = b.Watches(&autoscalingv2.HorizontalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(servicesForHPA))
		// Only when the kind exists: watching a missing CRD fails startup.
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(agent.ScaledObjectGVK)
		// By name rather than Owns: a conflicting ScaledObject we do not own must
		// also bring the service back when it is deleted.
		b = b.Watches(so, handler.EnqueueRequestsFromMapFunc(servicesForScaledObject))
	}
	return b.Named("agentservice").Complete(r)
}

// servicesForSession enqueues the AgentService a changed AgentSession is (or
// was) bound to. Finished and deleted sessions keep status.assignedAgent, so the
// service is refreshed when a session releases its capacity.
func servicesForSession(_ context.Context, o client.Object) []reconcile.Request {
	sess, ok := o.(*agentsv1alpha1.AgentSession)
	if !ok || sess.Status.AssignedAgent == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: sess.Namespace, Name: sess.Status.AssignedAgent,
	}}}
}

// servicesForRuntime enqueues every AgentService referencing a changed AgentRuntime.
func (r *AgentServiceReconciler) servicesForRuntime(ctx context.Context, o client.Object) []reconcile.Request {
	var list agentsv1alpha1.AgentServiceList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()),
		client.MatchingFields{indexServiceRuntimeRef: o.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing AgentServices for runtime", "runtime", o.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return reqs
}
