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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

func member(name string, ready int32) agentsv1alpha1.AgentService {
	return agentsv1alpha1.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     agentsv1alpha1.AgentServiceStatus{ReadyReplicas: ready},
	}
}

func TestPickAgent(t *testing.T) {
	tests := []struct {
		name    string
		members []agentsv1alpha1.AgentService
		active  map[string]int32
		per     int32
		want    string
	}{
		{"empty pool", nil, nil, 5, ""},
		{"no ready replicas", []agentsv1alpha1.AgentService{member("a", 0)}, nil, 5, ""},
		{"tie broken by name", []agentsv1alpha1.AgentService{member("b", 1), member("a", 1)}, nil, 5, "a"},
		{"least loaded wins", []agentsv1alpha1.AgentService{member("a", 1), member("b", 1)}, map[string]int32{"a": 2, "b": 1}, 5, "b"},
		{"ratio not absolute load", []agentsv1alpha1.AgentService{member("a", 4), member("b", 1)}, map[string]int32{"a": 4, "b": 2}, 5, "a"},
		{"full agents skipped", []agentsv1alpha1.AgentService{member("a", 1), member("b", 1)}, map[string]int32{"a": 1, "b": 1}, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickAgent(tt.members, tt.active, tt.per); got != tt.want {
				t.Errorf("pickAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestActiveSessionsByAgent(t *testing.T) {
	sessions := []agentsv1alpha1.AgentSession{
		{Status: agentsv1alpha1.AgentSessionStatus{Phase: agentsv1alpha1.AgentSessionBound, AssignedAgent: "a"}},
		{Status: agentsv1alpha1.AgentSessionStatus{Phase: agentsv1alpha1.AgentSessionBound, AssignedAgent: "a"}},
		{Status: agentsv1alpha1.AgentSessionStatus{Phase: agentsv1alpha1.AgentSessionCompleted, AssignedAgent: "a"}},
		{Status: agentsv1alpha1.AgentSessionStatus{Phase: agentsv1alpha1.AgentSessionPending}},
	}
	got := activeSessionsByAgent(sessions)
	if got["a"] != 2 || len(got) != 1 {
		t.Errorf("activeSessionsByAgent() = %v, want map[a:2]", got)
	}
}
