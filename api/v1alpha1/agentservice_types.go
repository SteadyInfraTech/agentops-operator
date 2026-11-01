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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentServicePhase is a high-level summary of an AgentService lifecycle.
// +kubebuilder:validation:Enum=Pending;Progressing;Running;Degraded;Failed
type AgentServicePhase string

const (
	AgentServicePending     AgentServicePhase = "Pending"
	AgentServiceProgressing AgentServicePhase = "Progressing"
	AgentServiceRunning     AgentServicePhase = "Running"
	AgentServiceDegraded    AgentServicePhase = "Degraded"
	AgentServiceFailed      AgentServicePhase = "Failed"
)

// AgentServiceSpec defines the desired state of an AI agent microservice.
// Either image or runtimeRef must be set.
// +kubebuilder:validation:XValidation:rule="has(self.image) || has(self.runtimeRef)",message="one of image or runtimeRef is required"
type AgentServiceSpec struct {
	// RuntimeRef references an AgentRuntime in the same namespace that supplies
	// defaults (image, port, probes, resources, env).
	// +optional
	RuntimeRef *LocalObjectReference `json:"runtimeRef,omitempty"`

	// Image overrides the runtime image.
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy for the agent container.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Command overrides the image entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args passed to the entrypoint.
	// +optional
	Args []string `json:"args,omitempty"`

	// Port the agent listens on. Defaults to the runtime port, or 8080.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Replicas is the static replica count, used when autoscaling is disabled.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Model configures the LLM used by the agent.
	// +optional
	Model *ModelSpec `json:"model,omitempty"`

	// SystemPrompt is passed to the agent as AGENT_SYSTEM_PROMPT.
	// Do not place secrets here; it is stored in plaintext.
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Tools is the list of tool identifiers the agent is allowed to use,
	// passed as a comma-separated AGENT_TOOLS variable.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Tools []string `json:"tools,omitempty"`

	// Env is appended after runtime env; entries with the same name override.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom injects ConfigMaps/Secrets as environment variables.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Resources overrides the runtime resources when set.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Autoscaling configures an HPA for this agent.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Probes overrides runtime probe settings.
	// +optional
	Probes *ProbeSpec `json:"probes,omitempty"`

	// Metrics overrides runtime metrics settings.
	// +optional
	Metrics *MetricsSpec `json:"metrics,omitempty"`

	// ServiceAccountName for agent pods. When empty the namespace default is
	// used. The API token is only mounted when a service account is set.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// ServiceType of the generated Service.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// NodeSelector for agent pods (e.g. to target GPU nodes).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations for agent pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// AgentServiceStatus defines the observed state of AgentService.
type AgentServiceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase AgentServicePhase `json:"phase,omitempty"`

	// Replicas is the current number of agent pods.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of agent pods passing readiness checks.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint is the in-cluster URL of the agent.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Image is the resolved image currently deployed.
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:shortName=agsvc;agent
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentService is the Schema for the agentservices API. It is the primary
// AgentOps resource: one AgentService becomes a Deployment, a Service and
// (optionally) a HorizontalPodAutoscaler.
type AgentService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentServiceSpec   `json:"spec,omitempty"`
	Status AgentServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentServiceList contains a list of AgentService.
type AgentServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentService{}, &AgentServiceList{})
}
