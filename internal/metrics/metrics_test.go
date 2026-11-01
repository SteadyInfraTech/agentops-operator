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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// resetAssignments drops every assignment series of a test namespace, now and
// when the test ends. The counters live in a process-wide registry, so without
// this a second run in the same process (go test -count=2) would see the
// first run's values.
func resetAssignments(t *testing.T, namespace string) {
	t.Helper()
	drop := func() {
		AgentSessionAssignments.DeletePartialMatch(prometheus.Labels{"namespace": namespace})
	}
	drop()
	t.Cleanup(drop)
}

// assignments returns the value of agentops_agentsession_assignments_total for
// the given label values and whether such a series exists.
func assignments(t *testing.T, namespace, target, result string) (float64, bool) {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	want := map[string]string{"namespace": namespace, "target": target, "result": result}
	for _, mf := range mfs {
		if mf.GetName() != "agentops_agentsession_assignments_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := 0
			for _, l := range m.GetLabel() {
				if want[l.GetName()] == l.GetValue() {
					matched++
				}
			}
			if matched == len(want) {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// gaugeValue returns the value of the named gauge series carrying the given
// labels and whether such a series exists.
func gaugeValue(t *testing.T, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := 0
			for _, l := range m.GetLabel() {
				if want[l.GetName()] == l.GetValue() {
					matched++
				}
			}
			if matched == len(want) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestDeleteAgentServiceDropsActiveSessionsSeries(t *testing.T) {
	const ns, gone, kept = "metrics-test-active", "gone", "kept"
	const name = "agentops_agentservice_active_sessions"
	AgentServiceActiveSessions.WithLabelValues(ns, gone).Set(3)
	AgentServiceActiveSessions.WithLabelValues(ns, kept).Set(2)
	t.Cleanup(func() {
		AgentServiceActiveSessions.DeletePartialMatch(prometheus.Labels{"namespace": ns})
	})

	if got, ok := gaugeValue(t, name, map[string]string{"namespace": ns, "name": gone}); !ok || got != 3 {
		t.Fatalf("active sessions for %s/%s = %v (exists=%v), want 3", ns, gone, got, ok)
	}

	DeleteAgentService(ns, gone)

	if _, ok := gaugeValue(t, name, map[string]string{"namespace": ns, "name": gone}); ok {
		t.Errorf("series for deleted AgentService %s/%s still exported", ns, gone)
	}
	if got, ok := gaugeValue(t, name, map[string]string{"namespace": ns, "name": kept}); !ok || got != 2 {
		t.Errorf("deleting %s/%s changed %s/%s: value=%v exists=%v, want 2", ns, gone, ns, kept, got, ok)
	}
}

func TestRecordSessionAssignment(t *testing.T) {
	const ns = "metrics-test"
	resetAssignments(t, ns)

	RecordSessionAssignment(ns, TargetPool, AssignmentBound)
	RecordSessionAssignment(ns, TargetPool, AssignmentBound)
	RecordSessionAssignment(ns, TargetPool, AssignmentUnschedulable)
	RecordSessionAssignment(ns, TargetAgent, AssignmentBound)

	if got, ok := assignments(t, ns, TargetPool, AssignmentBound); !ok || got != 2 {
		t.Errorf("bound for pool targets in %s = %v (exists=%v), want 2", ns, got, ok)
	}
	if got, ok := assignments(t, ns, TargetPool, AssignmentUnschedulable); !ok || got != 1 {
		t.Errorf("unschedulable for pool targets in %s = %v (exists=%v), want 1", ns, got, ok)
	}
	if got, ok := assignments(t, ns, TargetAgent, AssignmentBound); !ok || got != 1 {
		t.Errorf("bound for agent targets in %s = %v (exists=%v), want 1", ns, got, ok)
	}
}

// Reconciling a pool that does not exist must not erase assignment outcomes:
// the pool controller also reconciles pool names referenced by sessions whose
// pool is missing, and those sessions are counted exactly once.
func TestDeleteAgentPoolKeepsAssignmentSeries(t *testing.T) {
	const ns = "metrics-test-delete"
	resetAssignments(t, ns)

	RecordSessionAssignment(ns, TargetPool, AssignmentUnschedulable)
	DeleteAgentPool(ns, "missing-pool")

	if got, ok := assignments(t, ns, TargetPool, AssignmentUnschedulable); !ok || got != 1 {
		t.Errorf("unschedulable for pool targets in %s = %v (exists=%v) after DeleteAgentPool, want 1", ns, got, ok)
	}
}
