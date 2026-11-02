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
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	ManagedBy      = "agentops-operator"

	containerName = "agent"
	portName      = "http"
	metricsName   = "metrics"

	// preStopSleepSeconds delays container shutdown after a pod is marked
	// terminating, so that its removal from the Service endpoints has time to
	// propagate before the agent stops accepting connections. It uses the
	// native sleep action, which needs no binary in the agent image and is
	// why the operator requires Kubernetes 1.30+ (beta there, GA in 1.34).
	// The time counts against the pod's termination grace period.
	preStopSleepSeconds int64 = 5
)

// SelectorLabels are the immutable labels used to select agent pods.
func SelectorLabels(svc *agentsv1alpha1.AgentService) map[string]string {
	return map[string]string{
		LabelName:      "agent",
		LabelInstance:  svc.Name,
		LabelManagedBy: ManagedBy,
	}
}

// Labels are applied to every generated object.
func Labels(svc *agentsv1alpha1.AgentService) map[string]string {
	l := SelectorLabels(svc)
	l[LabelComponent] = "agent-service"
	return l
}

// Endpoint returns the in-cluster URL for an AgentService.
func Endpoint(svc *agentsv1alpha1.AgentService, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, port)
}

// MutateDeployment sets the desired state on dep in place. It only touches
// fields the operator owns so that server-side defaults are preserved and
// reconciles converge without spurious updates.
func MutateDeployment(dep *appsv1.Deployment, svc *agentsv1alpha1.AgentService, r Resolved) {
	dep.Labels = mergeMap(dep.Labels, Labels(svc))

	// When the HPA owns scaling we must not fight it: only seed replicas on create.
	if HPAEnabled(svc) {
		if dep.Spec.Replicas == nil {
			dep.Spec.Replicas = ptr.To(svc.Spec.Autoscaling.MinReplicas)
		}
	} else {
		dep.Spec.Replicas = ptr.To(ptr.Deref(svc.Spec.Replicas, 1))
	}

	// Selector is immutable after creation.
	if dep.Spec.Selector == nil {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: SelectorLabels(svc)}
	}

	// A rollout must never lower serving capacity: start a replacement before
	// retiring an old pod. Kubernetes' default (maxUnavailable 25%) would take
	// pods, and the sessions on them, away first.
	dep.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
			MaxSurge:       ptr.To(intstr.FromString("25%")),
		},
	}

	tpl := &dep.Spec.Template
	tpl.Labels = mergeMap(tpl.Labels, Labels(svc))
	tpl.Annotations = podAnnotations(tpl.Annotations, r)

	ps := &tpl.Spec
	ps.ServiceAccountName = svc.Spec.ServiceAccountName
	// Least privilege: agents rarely need the Kubernetes API.
	ps.AutomountServiceAccountToken = ptr.To(svc.Spec.ServiceAccountName != "")
	ps.NodeSelector = svc.Spec.NodeSelector
	ps.Tolerations = svc.Spec.Tolerations
	ps.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	c := corev1.Container{
		Name:            containerName,
		Image:           r.Image,
		ImagePullPolicy: r.ImagePullPolicy,
		Command:         r.Command,
		Args:            r.Args,
		Env:             r.Env,
		EnvFrom:         svc.Spec.EnvFrom,
		Resources:       r.Resources,
		Ports:           containerPorts(r),
		LivenessProbe:   httpProbe(r.Probes.LivenessPath, 10, 3),
		ReadinessProbe:  httpProbe(r.Probes.ReadinessPath, 5, 3),
		StartupProbe:    httpProbe(r.Probes.LivenessPath, 5, r.Probes.StartupFailureThreshold),
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: preStopSleepSeconds}},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	// Agents usually need scratch space (caches, temp files) despite the
	// read-only root filesystem.
	c.VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	ps.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	ps.Containers = []corev1.Container{c}
}

// MutateService sets the desired state on s in place.
func MutateService(s *corev1.Service, svc *agentsv1alpha1.AgentService, r Resolved) {
	s.Labels = mergeMap(s.Labels, Labels(svc))
	s.Spec.Selector = SelectorLabels(svc)
	s.Spec.Type = svc.Spec.ServiceType
	if s.Spec.Type == "" {
		s.Spec.Type = corev1.ServiceTypeClusterIP
	}
	ports := []corev1.ServicePort{{
		Name: portName, Port: r.Port, TargetPort: intstr.FromString(portName), Protocol: corev1.ProtocolTCP,
	}}
	if r.MetricsEnabled && r.MetricsPort != r.Port {
		ports = append(ports, corev1.ServicePort{
			Name: metricsName, Port: r.MetricsPort, TargetPort: intstr.FromString(metricsName), Protocol: corev1.ProtocolTCP,
		})
	}
	// Preserve allocated NodePorts to avoid needless reallocation on update.
	for i := range ports {
		for _, existing := range s.Spec.Ports {
			if existing.Name == ports[i].Name {
				ports[i].NodePort = existing.NodePort
			}
		}
	}
	s.Spec.Ports = ports
}

// MutateHPA sets the desired state on hpa in place. Callers must only invoke
// it when autoscaling is enabled.
func MutateHPA(hpa *autoscalingv2.HorizontalPodAutoscaler, svc *agentsv1alpha1.AgentService) {
	a := svc.Spec.Autoscaling
	hpa.Labels = mergeMap(hpa.Labels, Labels(svc))
	hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: svc.Name,
	}
	hpa.Spec.MinReplicas = ptr.To(a.MinReplicas)
	hpa.Spec.MaxReplicas = a.MaxReplicas

	var metrics []autoscalingv2.MetricSpec
	add := func(res corev1.ResourceName, target *int32) {
		if target == nil {
			return
		}
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   res,
				Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(*target)},
			},
		})
	}
	cpu := a.TargetCPUUtilizationPercentage
	if cpu == nil && a.TargetMemoryUtilizationPercentage == nil {
		cpu = ptr.To[int32](75)
	}
	add(corev1.ResourceCPU, cpu)
	add(corev1.ResourceMemory, a.TargetMemoryUtilizationPercentage)
	hpa.Spec.Metrics = metrics
}

// HPAEnabled reports whether the AgentService requests autoscaling.
func HPAEnabled(svc *agentsv1alpha1.AgentService) bool {
	return svc.Spec.Autoscaling != nil && svc.Spec.Autoscaling.Enabled
}

func containerPorts(r Resolved) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{{Name: portName, ContainerPort: r.Port, Protocol: corev1.ProtocolTCP}}
	if r.MetricsEnabled && r.MetricsPort != r.Port {
		ports = append(ports, corev1.ContainerPort{Name: metricsName, ContainerPort: r.MetricsPort, Protocol: corev1.ProtocolTCP})
	}
	return ports
}

func httpProbe(path string, period, failure int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromString(portName), Scheme: corev1.URISchemeHTTP,
		}},
		PeriodSeconds:    period,
		TimeoutSeconds:   2,
		SuccessThreshold: 1,
		FailureThreshold: failure,
	}
}

func podAnnotations(existing map[string]string, r Resolved) map[string]string {
	out := make(map[string]string, len(existing)+3)
	for k, v := range existing {
		out[k] = v
	}
	if r.MetricsEnabled {
		out["prometheus.io/scrape"] = "true"
		out["prometheus.io/port"] = strconv.Itoa(int(r.MetricsPort))
		out["prometheus.io/path"] = r.MetricsPath
	} else {
		delete(out, "prometheus.io/scrape")
		delete(out, "prometheus.io/port")
		delete(out, "prometheus.io/path")
	}
	return out
}

func mergeMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
