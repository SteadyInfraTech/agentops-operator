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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// ScaledObjectGVK is the KEDA ScaledObject kind. It is handled as unstructured
// so the operator takes no dependency on KEDA's Go module.
var ScaledObjectGVK = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

// NewScaledObject returns an empty ScaledObject for svc, ready for CreateOrUpdate.
func NewScaledObject(svc *agentsv1alpha1.AgentService) *unstructured.Unstructured {
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(ScaledObjectGVK)
	so.SetName(svc.Name)
	so.SetNamespace(svc.Namespace)
	return so
}

// SessionScalingRequested reports whether svc asks to scale on active sessions.
func SessionScalingRequested(svc *agentsv1alpha1.AgentService) bool {
	return HPAEnabled(svc) && svc.Spec.Autoscaling.TargetSessionsPerPod != nil
}

// SessionsQuery is the PromQL that yields the active sessions of svc. It relies
// on the namespace and name labels of agentops_agentservice_active_sessions
// surviving the scrape (the shipped ServiceMonitor sets honorLabels).
func SessionsQuery(svc *agentsv1alpha1.AgentService) string {
	return fmt.Sprintf(`sum(agentops_agentservice_active_sessions{namespace=%q,name=%q})`, svc.Namespace, svc.Name)
}

// MutateScaledObject writes the desired spec of the ScaledObject for svc.
//
// It sets individual spec keys instead of replacing spec, so fields KEDA or an
// admission webhook adds are kept and an unchanged desired state compares
// equal. Triggers mirror the HPA: the largest recommendation wins, so a busy
// CPU still scales up even when the session count is low. The prometheus
// threshold is an average per pod, which is what TargetSessionsPerPod means.
func MutateScaledObject(so *unstructured.Unstructured, svc *agentsv1alpha1.AgentService, prometheusAddress string) error {
	a := svc.Spec.Autoscaling
	so.SetLabels(mergeMap(so.GetLabels(), Labels(svc)))

	triggers := []interface{}{map[string]interface{}{
		"type": "prometheus",
		"metadata": map[string]interface{}{
			"serverAddress": prometheusAddress,
			"query":         SessionsQuery(svc),
			"threshold":     fmt.Sprint(*a.TargetSessionsPerPod),
			// No series yet (new service, no sessions) means zero, not an error.
			"ignoreNullValues": "true",
		},
	}}
	resource := func(name string, target *int32) {
		if target == nil {
			return
		}
		triggers = append(triggers, map[string]interface{}{
			"type":       name,
			"metricType": "Utilization",
			"metadata":   map[string]interface{}{"value": fmt.Sprint(*target)},
		})
	}
	cpu, memory := resourceTargets(a)
	resource("cpu", cpu)
	resource("memory", memory)

	set := func(value interface{}, path ...string) error {
		return unstructured.SetNestedField(so.Object, value, append([]string{"spec"}, path...)...)
	}
	if err := set(map[string]interface{}{"name": svc.Name}, "scaleTargetRef"); err != nil {
		return err
	}
	if err := set(int64(a.MinReplicas), "minReplicaCount"); err != nil {
		return err
	}
	if err := set(int64(a.MaxReplicas), "maxReplicaCount"); err != nil {
		return err
	}
	if err := set(triggers, "triggers"); err != nil {
		return err
	}

	if state, _ := SessionModel(svc); state == agentsv1alpha1.SessionStateless {
		unstructured.RemoveNestedField(so.Object, "spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleDown")
		return nil
	}
	rules, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulScaleDownRules())
	if err != nil {
		return fmt.Errorf("converting scale-down rules: %w", err)
	}
	return set(rules, "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleDown")
}
