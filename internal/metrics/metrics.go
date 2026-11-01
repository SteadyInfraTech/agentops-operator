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

// Package metrics defines AgentOps-specific Prometheus metrics. They are
// registered with controller-runtime's registry and therefore served on the
// manager's (authenticated) metrics endpoint alongside the built-in
// controller_runtime_* metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// AgentServiceReplicas reports desired/current/ready replicas per AgentService.
	AgentServiceReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agentops_agentservice_replicas",
		Help: "Replica counts of AgentService deployments by state (desired, current, ready).",
	}, []string{"namespace", "name", "state"})

	// AgentServicePhase is 1 for the current phase of an AgentService and 0 otherwise.
	AgentServicePhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agentops_agentservice_phase",
		Help: "Current phase of an AgentService (1 = active phase).",
	}, []string{"namespace", "name", "phase"})

	// AgentPoolSessions reports active sessions and capacity per AgentPool.
	AgentPoolSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agentops_agentpool_sessions",
		Help: "Session counts of an AgentPool by kind (active, capacity).",
	}, []string{"namespace", "name", "kind"})

	// AgentSessionTransitions counts AgentSession phase transitions.
	AgentSessionTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentops_agentsession_transitions_total",
		Help: "Number of AgentSession phase transitions by target phase.",
	}, []string{"namespace", "phase"})

	// AgentSessionAssignments counts AgentSession scheduling outcomes per pool.
	// "bound" is recorded once per successful placement and "unschedulable"
	// once each time a session enters the unschedulable state (not on every
	// retry), so the rate reflects sessions rather than requeues.
	AgentSessionAssignments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentops_agentsession_assignments_total",
		Help: "Number of AgentSession scheduling outcomes by pool and result (bound, unschedulable).",
	}, []string{"namespace", "pool", "result"})
)

// Values of the "result" label of AgentSessionAssignments.
const (
	// AssignmentBound means the session was placed onto an AgentService.
	AssignmentBound = "bound"
	// AssignmentUnschedulable means no target had free capacity.
	AssignmentUnschedulable = "unschedulable"
)

// DirectPool is the "pool" label value for sessions pinned to an AgentService
// through spec.agentServiceRef. It is not a valid Kubernetes object name, so it
// cannot collide with a real AgentPool.
const DirectPool = "-"

func init() {
	metrics.Registry.MustRegister(
		AgentServiceReplicas, AgentServicePhase, AgentPoolSessions,
		AgentSessionTransitions, AgentSessionAssignments,
	)
}

// RecordSessionAssignment records one scheduling outcome for a session in the
// given pool (use DirectPool for sessions pinned to an AgentService).
func RecordSessionAssignment(namespace, pool, result string) {
	AgentSessionAssignments.WithLabelValues(namespace, pool, result).Inc()
}

// DeleteAgentService removes all series for a deleted AgentService so that
// cardinality does not grow unbounded.
func DeleteAgentService(namespace, name string) {
	l := prometheus.Labels{"namespace": namespace, "name": name}
	AgentServiceReplicas.DeletePartialMatch(l)
	AgentServicePhase.DeletePartialMatch(l)
}

// DeleteAgentPool removes all series for a deleted AgentPool.
func DeleteAgentPool(namespace, name string) {
	AgentPoolSessions.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	AgentSessionAssignments.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "pool": name})
}
