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

// AgentPoolSpec groups AgentServices into a logical pool that AgentSessions
// can be scheduled onto.
type AgentPoolSpec struct {
	// Selector matches AgentServices (in the pool's namespace) that are members.
	Selector metav1.LabelSelector `json:"selector"`

	// MaxSessionsPerReplica caps concurrent sessions per ready agent replica.
	// Pool capacity = sum(readyReplicas) * maxSessionsPerReplica.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	// +optional
	MaxSessionsPerReplica int32 `json:"maxSessionsPerReplica,omitempty"`
}

// AgentPoolMember summarises a member AgentService.
type AgentPoolMember struct {
	Name           string `json:"name"`
	ReadyReplicas  int32  `json:"readyReplicas"`
	ActiveSessions int32  `json:"activeSessions"`
}

// AgentPoolStatus defines the observed state of AgentPool.
type AgentPoolStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Agents is the number of member AgentServices.
	// +optional
	Agents int32 `json:"agents,omitempty"`

	// ReadyAgents is the number of members with at least one ready replica.
	// +optional
	ReadyAgents int32 `json:"readyAgents,omitempty"`

	// Capacity is the total number of sessions the pool can host.
	// +optional
	Capacity int32 `json:"capacity,omitempty"`

	// ActiveSessions is the number of bound sessions.
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty"`

	// +optional
	Members []AgentPoolMember `json:"members,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=apool
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agents`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyAgents`
// +kubebuilder:printcolumn:name="Sessions",type=integer,JSONPath=`.status.activeSessions`
// +kubebuilder:printcolumn:name="Capacity",type=integer,JSONPath=`.status.capacity`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentPool is a group of AgentServices that share session capacity.
type AgentPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentPoolSpec   `json:"spec,omitempty"`
	Status AgentPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentPoolList contains a list of AgentPool.
type AgentPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentPool{}, &AgentPoolList{})
}
