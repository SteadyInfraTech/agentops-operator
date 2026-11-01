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
	opmetrics "github.com/SteadyInfraTech/agentops-operator/internal/metrics"
)

func sessionWith(phase agentsv1alpha1.AgentSessionPhase, readyReason string) *agentsv1alpha1.AgentSession {
	s := &agentsv1alpha1.AgentSession{}
	s.Status.Phase = phase
	if readyReason != "" {
		s.Status.Conditions = []metav1.Condition{{
			Type: agentsv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: readyReason,
		}}
	}
	return s
}

func TestAssignmentOutcome(t *testing.T) {
	cases := []struct {
		name      string
		orig, now *agentsv1alpha1.AgentSession
		want      string
	}{
		{
			name: "new session binds",
			orig: sessionWith("", ""),
			now:  sessionWith(agentsv1alpha1.AgentSessionBound, "Bound"),
			want: opmetrics.AssignmentBound,
		},
		{
			name: "pending session binds",
			orig: sessionWith(agentsv1alpha1.AgentSessionPending, reasonUnschedulable),
			now:  sessionWith(agentsv1alpha1.AgentSessionBound, "Bound"),
			want: opmetrics.AssignmentBound,
		},
		{
			name: "new session becomes unschedulable",
			orig: sessionWith("", ""),
			now:  sessionWith(agentsv1alpha1.AgentSessionPending, reasonUnschedulable),
			want: opmetrics.AssignmentUnschedulable,
		},
		{
			name: "retry of an already unschedulable session is not counted again",
			orig: sessionWith(agentsv1alpha1.AgentSessionPending, reasonUnschedulable),
			now:  sessionWith(agentsv1alpha1.AgentSessionPending, reasonUnschedulable),
			want: "",
		},
		{
			name: "bound session staying bound is not counted again",
			orig: sessionWith(agentsv1alpha1.AgentSessionBound, "Bound"),
			now:  sessionWith(agentsv1alpha1.AgentSessionBound, "Bound"),
			want: "",
		},
		{
			name: "completed before binding is not an assignment",
			orig: sessionWith("", ""),
			now:  sessionWith(agentsv1alpha1.AgentSessionCompleted, "Completed"),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assignmentOutcome(tc.orig, tc.now); got != tc.want {
				t.Errorf("assignmentOutcome() = %q, want %q", got, tc.want)
			}
		})
	}
}
