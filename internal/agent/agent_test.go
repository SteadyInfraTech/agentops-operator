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

package agent

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

func envMap(env []corev1.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

func TestResolveServiceOverridesRuntime(t *testing.T) {
	rt := &agentsv1alpha1.AgentRuntime{Spec: agentsv1alpha1.AgentRuntimeSpec{
		Image: "rt:1", Port: 9000,
		Env:     []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}, {Name: "KEEP", Value: "x"}},
		Probes:  &agentsv1alpha1.ProbeSpec{LivenessPath: "/live"},
		Metrics: &agentsv1alpha1.MetricsSpec{Port: 9100},
	}}
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: agentsv1alpha1.AgentServiceSpec{
			Image:  "svc:2",
			Env:    []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}},
			Tools:  []string{"search", "code"},
			Model:  &agentsv1alpha1.ModelSpec{Provider: "anthropic", Name: "claude"},
			Probes: &agentsv1alpha1.ProbeSpec{ReadinessPath: "/ready"},
		},
	}

	r := Resolve(svc, rt)
	if r.Image != "svc:2" || r.Port != 9000 {
		t.Fatalf("image/port = %s/%d", r.Image, r.Port)
	}
	env := envMap(r.Env)
	for k, want := range map[string]string{
		"LOG_LEVEL": "debug", "KEEP": "x", "AGENT_TOOLS": "search,code",
		"AGENT_MODEL_PROVIDER": "anthropic", "AGENT_NAME": "a",
	} {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}
	if r.Probes.LivenessPath != "/live" || r.Probes.ReadinessPath != "/ready" ||
		r.Probes.StartupFailureThreshold != DefaultStartupFailureThreshold {
		t.Errorf("probes = %+v", r.Probes)
	}
	if !r.MetricsEnabled || r.MetricsPort != 9100 || r.MetricsPath != DefaultMetricsPath {
		t.Errorf("metrics = %v %d %s", r.MetricsEnabled, r.MetricsPort, r.MetricsPath)
	}
}

func TestResolveDefaultsWithoutRuntime(t *testing.T) {
	r := Resolve(&agentsv1alpha1.AgentService{Spec: agentsv1alpha1.AgentServiceSpec{Image: "x"}}, nil)
	if r.Port != DefaultPort || r.MetricsPort != DefaultPort || r.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("unexpected defaults: %+v", r)
	}
}

func TestMergeEnvIsOrderStable(t *testing.T) {
	got := mergeEnv(
		[]corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}},
		[]corev1.EnvVar{{Name: "C", Value: "3"}, {Name: "A", Value: "9"}},
	)
	want := []corev1.EnvVar{{Name: "A", Value: "9"}, {Name: "B", Value: "2"}, {Name: "C", Value: "3"}}
	if !equality.Semantic.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %v, want %v", got, want)
	}
}

func TestMutateDeploymentRespectsHPA(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: agentsv1alpha1.AgentServiceSpec{
			Image:       "x",
			Replicas:    ptr.To[int32](7),
			Autoscaling: &agentsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 5},
		},
	}
	r := Resolve(svc, nil)

	dep := &appsv1.Deployment{}
	MutateDeployment(dep, svc, r)
	if *dep.Spec.Replicas != 2 {
		t.Fatalf("initial replicas = %d, want minReplicas 2", *dep.Spec.Replicas)
	}
	// Simulate the HPA scaling up; a subsequent reconcile must not reset it.
	dep.Spec.Replicas = ptr.To[int32](4)
	MutateDeployment(dep, svc, r)
	if *dep.Spec.Replicas != 4 {
		t.Fatalf("replicas reset to %d; operator must not fight the HPA", *dep.Spec.Replicas)
	}

	svc.Spec.Autoscaling.Enabled = false
	MutateDeployment(dep, svc, r)
	if *dep.Spec.Replicas != 7 {
		t.Fatalf("static replicas = %d, want 7", *dep.Spec.Replicas)
	}
}

func TestMutateDeploymentRolloutAndTermination(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec:       agentsv1alpha1.AgentServiceSpec{Image: "x"},
	}
	r := Resolve(svc, nil)
	dep := &appsv1.Deployment{}
	MutateDeployment(dep, svc, r)

	st := dep.Spec.Strategy
	if st.Type != appsv1.RollingUpdateDeploymentStrategyType || st.RollingUpdate == nil {
		t.Fatalf("strategy = %+v, want RollingUpdate", st)
	}
	if got := st.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 0 {
		t.Errorf("maxUnavailable = %v, want 0 so a rollout never lowers capacity", got)
	}
	if got := st.RollingUpdate.MaxSurge; got == nil || got.String() != "25%" {
		t.Errorf("maxSurge = %v, want 25%% (must be non-zero when maxUnavailable is 0)", got)
	}

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Sleep == nil {
		t.Fatalf("lifecycle = %+v, want a native preStop sleep action", c.Lifecycle)
	}
	if got := c.Lifecycle.PreStop.Sleep.Seconds; got != preStopSleepSeconds {
		t.Errorf("preStop sleep = %ds, want %ds", got, preStopSleepSeconds)
	}
	if c.Lifecycle.PreStop.Exec != nil || c.Lifecycle.PreStop.HTTPGet != nil {
		t.Errorf("preStop must use only the native sleep: the image may not contain a shell or sleep binary")
	}

	// The grace period is left to Kubernetes' default until draining is opt-in:
	// raising it would make rollouts wait for any agent that ignores SIGTERM.
	if dep.Spec.Template.Spec.TerminationGracePeriodSeconds != nil {
		t.Errorf("terminationGracePeriodSeconds = %d, want unset (Kubernetes default)",
			*dep.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}

	// Reconciling again must not change anything, or CreateOrUpdate would
	// issue an update on every pass.
	before := dep.DeepCopy()
	MutateDeployment(dep, svc, r)
	if !equality.Semantic.DeepEqual(before.Spec, dep.Spec) {
		t.Errorf("MutateDeployment is not idempotent")
	}
}

func TestMutateDeploymentMetricsToggle(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec:       agentsv1alpha1.AgentServiceSpec{Image: "x", Metrics: &agentsv1alpha1.MetricsSpec{Port: 9100}},
	}
	dep := &appsv1.Deployment{}
	MutateDeployment(dep, svc, Resolve(svc, nil))
	if dep.Spec.Template.Annotations["prometheus.io/port"] != "9100" {
		t.Errorf("annotations = %v", dep.Spec.Template.Annotations)
	}
	if n := len(dep.Spec.Template.Spec.Containers[0].Ports); n != 2 {
		t.Errorf("container ports = %d, want 2 (http + metrics)", n)
	}

	svc.Spec.Metrics.Enabled = ptr.To(false)
	MutateDeployment(dep, svc, Resolve(svc, nil))
	if _, ok := dep.Spec.Template.Annotations["prometheus.io/scrape"]; ok {
		t.Errorf("scrape annotation should be removed when metrics are disabled")
	}
}

func TestMutateServicePreservesNodePort(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec:       agentsv1alpha1.AgentServiceSpec{Image: "x", ServiceType: corev1.ServiceTypeNodePort},
	}
	s := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", NodePort: 31000}}}}
	MutateService(s, svc, Resolve(svc, nil))
	if s.Spec.Ports[0].NodePort != 31000 {
		t.Errorf("NodePort = %d, want preserved 31000", s.Spec.Ports[0].NodePort)
	}
}

func TestMutateHPADefaultsToCPU(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       agentsv1alpha1.AgentServiceSpec{Autoscaling: &agentsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 3}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	MutateHPA(hpa, svc)
	if len(hpa.Spec.Metrics) != 1 || hpa.Spec.Metrics[0].Resource.Name != corev1.ResourceCPU {
		t.Errorf("metrics = %+v", hpa.Spec.Metrics)
	}
	if hpa.Spec.ScaleTargetRef.Name != "a" || hpa.Spec.MaxReplicas != 3 {
		t.Errorf("hpa spec = %+v", hpa.Spec)
	}
}

func TestMutateHPAScaleBehaviorFollowsSessionState(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       agentsv1alpha1.AgentServiceSpec{Autoscaling: &agentsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 3}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}

	MutateHPA(hpa, svc) // Stateful is the default
	sd := hpa.Spec.Behavior.ScaleDown
	if sd == nil || ptr.Deref(sd.StabilizationWindowSeconds, 0) != statefulScaleDownWindowSeconds {
		t.Fatalf("stateful scaleDown = %+v", sd)
	}
	if len(sd.Policies) != 1 || sd.Policies[0].Type != autoscalingv2.PodsScalingPolicy ||
		sd.Policies[0].Value != 1 || sd.Policies[0].PeriodSeconds != statefulScaleDownPeriodSeconds {
		t.Errorf("stateful policies = %+v", sd.Policies)
	}
	if hpa.Spec.Behavior.ScaleUp != nil {
		t.Errorf("scale-up must stay unset when absent, got %+v", hpa.Spec.Behavior.ScaleUp)
	}

	// The API server defaults ScaleUp; reconciling must not touch it, or every
	// reconcile would look like a change.
	serverScaleUp := &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: ptr.To[int32](0)}
	hpa.Spec.Behavior.ScaleUp = serverScaleUp
	before := hpa.DeepCopy()
	MutateHPA(hpa, svc)
	if !equality.Semantic.DeepEqual(before.Spec, hpa.Spec) {
		t.Errorf("second mutate changed the spec:\n%+v\n%+v", before.Spec, hpa.Spec)
	}
	if hpa.Spec.Behavior.ScaleUp != serverScaleUp {
		t.Errorf("scale-up was replaced: %+v", hpa.Spec.Behavior.ScaleUp)
	}

	svc.Spec.Sessions = &agentsv1alpha1.SessionsSpec{State: agentsv1alpha1.SessionStateless}
	MutateHPA(hpa, svc)
	if hpa.Spec.Behavior != nil {
		t.Errorf("stateless should restore default behavior, got %+v", hpa.Spec.Behavior)
	}
}

func TestDrainSafeCondition(t *testing.T) {
	svc := func(s *agentsv1alpha1.SessionsSpec) *agentsv1alpha1.AgentService {
		a := &agentsv1alpha1.AgentService{}
		a.Generation = 3
		a.Spec.Sessions = s
		return a
	}
	tests := []struct {
		name       string
		sessions   *agentsv1alpha1.SessionsSpec
		wantReason string
	}{
		{"omitted is Stateful over Request", nil, agentsv1alpha1.ReasonRequestTransportWithoutRouting},
		{"empty object takes the same defaults", &agentsv1alpha1.SessionsSpec{}, agentsv1alpha1.ReasonRequestTransportWithoutRouting},
		{"stateless over request", &agentsv1alpha1.SessionsSpec{State: agentsv1alpha1.SessionStateless}, agentsv1alpha1.ReasonBestEffortOnly},
		{"stateful over connection", &agentsv1alpha1.SessionsSpec{Transport: agentsv1alpha1.TransportConnection}, agentsv1alpha1.ReasonDrainNotEnabled},
		{
			"stateless over connection",
			&agentsv1alpha1.SessionsSpec{State: agentsv1alpha1.SessionStateless, Transport: agentsv1alpha1.TransportConnection},
			agentsv1alpha1.ReasonDrainNotEnabled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DrainSafeCondition(svc(tt.sessions))
			if c.Type != agentsv1alpha1.ConditionDrainSafe {
				t.Errorf("type = %q", c.Type)
			}
			if c.Status != metav1.ConditionFalse {
				t.Errorf("status = %q, want False: no drain guarantee exists yet", c.Status)
			}
			if c.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", c.Reason, tt.wantReason)
			}
			if c.Message == "" || c.ObservedGeneration != 3 {
				t.Errorf("message %q, observedGeneration %d", c.Message, c.ObservedGeneration)
			}
		})
	}
}

func TestMutateScaledObject(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: agentsv1alpha1.AgentServiceSpec{Autoscaling: &agentsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: 2, MaxReplicas: 6,
			TargetCPUUtilizationPercentage: ptr.To[int32](75),
			TargetSessionsPerPod:           ptr.To[int32](8),
		}},
	}
	if !SessionScalingRequested(svc) {
		t.Fatal("target set and autoscaling enabled should request session scaling")
	}

	so := NewScaledObject(svc)
	// A field added by KEDA or a webhook must survive.
	_ = unstructured.SetNestedField(so.Object, int64(30), "spec", "pollingInterval")
	if err := MutateScaledObject(so, svc, "http://prom:9090"); err != nil {
		t.Fatal(err)
	}
	first := so.DeepCopy()
	if err := MutateScaledObject(so, svc, "http://prom:9090"); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(first.Object, so.Object) {
		t.Error("a second mutate must be a no-op")
	}
	if v, _, _ := unstructured.NestedInt64(so.Object, "spec", "pollingInterval"); v != 30 {
		t.Errorf("pollingInterval = %d, field set by someone else was dropped", v)
	}

	triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if len(triggers) != 2 {
		t.Fatalf("triggers = %v, want sessions + cpu", triggers)
	}
	md := triggers[0].(map[string]interface{})["metadata"].(map[string]interface{})
	if md["threshold"] != "8" || md["serverAddress"] != "http://prom:9090" ||
		md["query"] != `sum(agentops_agentservice_active_sessions{namespace="ns",name="a"})` {
		t.Errorf("prometheus trigger metadata = %v", md)
	}
	if triggers[1].(map[string]interface{})["type"] != "cpu" {
		t.Errorf("second trigger = %v", triggers[1])
	}
	if w, _, _ := unstructured.NestedInt64(so.Object, "spec", "advanced", "horizontalPodAutoscalerConfig",
		"behavior", "scaleDown", "stabilizationWindowSeconds"); w != int64(statefulScaleDownWindowSeconds) {
		t.Errorf("stateful scale-down window = %d", w)
	}

	svc.Spec.Sessions = &agentsv1alpha1.SessionsSpec{State: agentsv1alpha1.SessionStateless}
	if err := MutateScaledObject(so, svc, "http://prom:9090"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedMap(so.Object, "spec", "advanced", "horizontalPodAutoscalerConfig",
		"behavior", "scaleDown"); found {
		t.Error("stateless should drop the scale-down pacing")
	}

	svc.Spec.Autoscaling.TargetSessionsPerPod = nil
	if SessionScalingRequested(svc) {
		t.Error("no target means no session scaling")
	}
}

func TestScaledObjectKeepsTheHPADefaultCPUTarget(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: agentsv1alpha1.AgentServiceSpec{Autoscaling: &agentsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: 1, MaxReplicas: 3, TargetSessionsPerPod: ptr.To[int32](5),
		}},
	}
	so := NewScaledObject(svc)
	if err := MutateScaledObject(so, svc, "http://prom:9090"); err != nil {
		t.Fatal(err)
	}
	triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if len(triggers) != 2 {
		t.Fatalf("triggers = %v, want sessions + the default cpu", triggers)
	}
	cpu := triggers[1].(map[string]interface{})
	if cpu["type"] != "cpu" || cpu["metadata"].(map[string]interface{})["value"] != "75" {
		t.Errorf("default cpu trigger = %v, want 75%% like the HPA", cpu)
	}

	// Memory alone replaces the default, as it does for the HPA.
	svc.Spec.Autoscaling.TargetMemoryUtilizationPercentage = ptr.To[int32](60)
	if err := MutateScaledObject(so, svc, "http://prom:9090"); err != nil {
		t.Fatal(err)
	}
	triggers, _, _ = unstructured.NestedSlice(so.Object, "spec", "triggers")
	if len(triggers) != 2 || triggers[1].(map[string]interface{})["type"] != "memory" {
		t.Errorf("triggers = %v, want sessions + memory only", triggers)
	}
}
