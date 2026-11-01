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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSessionPhase is the lifecycle phase of an AgentSession.
// +kubebuilder:validation:Enum=Pending;Bound;Completed;Expired;Failed
type AgentSessionPhase string

const (
	// AgentSessionPending: waiting for a target with free capacity.
	AgentSessionPending AgentSessionPhase = "Pending"
	// AgentSessionBound: assigned to an AgentService.
	AgentSessionBound AgentSessionPhase = "Bound"
	// AgentSessionCompleted: explicitly finished by the client (spec.completed=true).
	AgentSessionCompleted AgentSessionPhase = "Completed"
	// AgentSessionExpired: exceeded spec.maxDurationSeconds.
	AgentSessionExpired AgentSessionPhase = "Expired"
	// AgentSessionFailed: target missing or invalid.
	AgentSessionFailed AgentSessionPhase = "Failed"
)

// AgentSessionSpec describes a long-running conversation/task bound to an agent.
// +kubebuilder:validation:XValidation:rule="has(self.poolRef) != has(self.agentServiceRef)",message="exactly one of poolRef or agentServiceRef is required"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.poolRef) || self.poolRef == oldSelf.poolRef",message="poolRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.agentServiceRef) || self.agentServiceRef == oldSelf.agentServiceRef",message="agentServiceRef is immutable"
type AgentSessionSpec struct {
	// PoolRef schedules the session onto the least-loaded agent in a pool.
	// +optional
	PoolRef *LocalObjectReference `json:"poolRef,omitempty"`

	// AgentServiceRef pins the session to a specific AgentService.
	// +optional
	AgentServiceRef *LocalObjectReference `json:"agentServiceRef,omitempty"`

	// UserID is an opaque identifier of the session owner.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	UserID string `json:"userID,omitempty"`

	// Parameters are free-form session parameters for the agent.
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// MaxDurationSeconds after binding before the session expires. 0 = no limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3600
	// +optional
	MaxDurationSeconds int64 `json:"maxDurationSeconds,omitempty"`

	// TTLSecondsAfterFinished deletes the session this long after it reaches a
	// terminal phase. Unset = keep forever.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`

	// Completed marks the session as finished, releasing its capacity.
	// +optional
	Completed bool `json:"completed,omitempty"`
}

// AgentSessionStatus defines the observed state of AgentSession.
type AgentSessionStatus struct {
	// +optional
	Phase AgentSessionPhase `json:"phase,omitempty"`

	// AssignedAgent is the AgentService the session is bound to.
	// +optional
	AssignedAgent string `json:"assignedAgent,omitempty"`

	// Endpoint of the assigned agent.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// +optional
	BoundAt *metav1.Time `json:"boundAt,omitempty"`

	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=asess
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.assignedAgent`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.userID`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentSession is a long-running session scheduled onto an AgentService.
type AgentSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSessionSpec   `json:"spec,omitempty"`
	Status AgentSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentSessionList contains a list of AgentSession.
type AgentSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSession `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSession{}, &AgentSessionList{})
}
