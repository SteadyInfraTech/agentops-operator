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
)

// Condition types shared by all AgentOps resources.
const (
	// ConditionReady indicates the resource is fully reconciled and serving.
	ConditionReady = "Ready"
	// ConditionProgressing indicates the resource is converging towards the desired state.
	ConditionProgressing = "Progressing"
	// ConditionDegraded indicates the resource is not operating as intended.
	ConditionDegraded = "Degraded"
	// ConditionDrainSafe indicates whether agent pods can be removed (scale-in,
	// rollout, node drain) without cutting live sessions. It is True only when
	// there is an explicit guarantee, so it never claims more than is delivered.
	ConditionDrainSafe = "DrainSafe"
)

// Reasons of the DrainSafe condition.
const (
	// ReasonBestEffortOnly: a Stateless agent reached by independent requests.
	// No session state is lost, but an in-flight request can be interrupted.
	ReasonBestEffortOnly = "BestEffortOnly"
	// ReasonDrainNotEnabled: sessions are long-lived connections and removing a
	// pod cuts them; draining is not enabled for the agent.
	ReasonDrainNotEnabled = "DrainNotEnabled"
	// ReasonRequestTransportWithoutRouting: a Stateful agent reached by
	// independent requests, which needs session-aware routing that does not
	// exist yet.
	ReasonRequestTransportWithoutRouting = "RequestTransportWithoutRouting"
)

// ModelSpec describes the LLM backing an agent. The operator does not call the
// model itself; the values are injected into the agent container as
// environment variables (AGENT_MODEL_PROVIDER, AGENT_MODEL_NAME, ...).
type ModelSpec struct {
	// Provider of the model, e.g. "openai", "anthropic", "vllm", "ollama".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Provider string `json:"provider"`

	// Name of the model, e.g. "gpt-4o" or "claude-sonnet-4".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Endpoint overrides the provider base URL (useful for self-hosted models).
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://.+`
	Endpoint string `json:"endpoint,omitempty"`

	// APIKeySecretRef references a Secret key holding the provider API key.
	// It is exposed to the container as AGENT_MODEL_API_KEY via secretKeyRef,
	// so the plaintext value never appears in the AgentService or Deployment spec.
	// +optional
	APIKeySecretRef *corev1.SecretKeySelector `json:"apiKeySecretRef,omitempty"`
}

// ProbeSpec configures HTTP health probes for an agent container.
type ProbeSpec struct {
	// LivenessPath is the HTTP path used by the liveness probe.
	// +kubebuilder:default="/healthz"
	// +optional
	LivenessPath string `json:"livenessPath,omitempty"`

	// ReadinessPath is the HTTP path used by the readiness probe.
	// +kubebuilder:default="/readyz"
	// +optional
	ReadinessPath string `json:"readinessPath,omitempty"`

	// StartupFailureThreshold * 5s is the maximum time an agent may take to
	// start (e.g. loading tools, warming caches) before it is restarted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=360
	// +kubebuilder:default=60
	// +optional
	StartupFailureThreshold int32 `json:"startupFailureThreshold,omitempty"`
}

// MetricsSpec configures Prometheus scraping of an agent.
type MetricsSpec struct {
	// Enabled toggles prometheus.io/* scrape annotations on agent pods and the
	// "metrics" port on the agent Service.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Port on which the agent exposes metrics. Defaults to the agent port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Path of the metrics endpoint.
	// +kubebuilder:default="/metrics"
	// +optional
	Path string `json:"path,omitempty"`
}

// AutoscalingSpec configures a HorizontalPodAutoscaler for an agent.
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be >= minReplicas"
type AutoscalingSpec struct {
	// Enabled creates an HPA for the agent Deployment.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// TargetCPUUtilizationPercentage is the average CPU utilization target.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=75
	// +optional
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// TargetMemoryUtilizationPercentage is the average memory utilization target.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetMemoryUtilizationPercentage *int32 `json:"targetMemoryUtilizationPercentage,omitempty"`

	// TargetSessionsPerPod scales on active sessions: the replica count follows
	// agentops_agentservice_active_sessions divided by this value.
	// It takes effect only when KEDA is installed and the operator has a
	// --prometheus-address; otherwise it is ignored and CPU/memory apply.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TargetSessionsPerPod *int32 `json:"targetSessionsPerPod,omitempty"`
}

// LocalObjectReference references an object in the same namespace.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SessionState says whether an agent keeps session state that is lost when its
// pod goes away.
// +kubebuilder:validation:Enum=Stateful;Stateless
type SessionState string

const (
	// SessionStateful: the agent may hold session state in memory. This is the
	// safe assumption and the default.
	SessionStateful SessionState = "Stateful"
	// SessionStateless: the agent holds no session state in memory.
	SessionStateless SessionState = "Stateless"
)

// SessionTransport says how a session reaches its agent.
// +kubebuilder:validation:Enum=Connection;Request
type SessionTransport string

const (
	// TransportConnection: a session is one long-lived connection (WebSocket,
	// SSE, gRPC stream).
	TransportConnection SessionTransport = "Connection"
	// TransportRequest: a session is a series of independent requests.
	TransportRequest SessionTransport = "Request"
)

// SessionsSpec describes how an agent holds and serves sessions, so the
// operator can report whether pods can be removed without cutting them.
type SessionsSpec struct {
	// State is whether the agent keeps session state in memory.
	// Defaults to Stateful, the safe assumption.
	// +kubebuilder:default=Stateful
	// +optional
	State SessionState `json:"state,omitempty"`

	// Transport is how a session reaches the agent: one long-lived
	// connection (Connection) or independent requests (Request).
	// +kubebuilder:default=Request
	// +optional
	Transport SessionTransport `json:"transport,omitempty"`
}
