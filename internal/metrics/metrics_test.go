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

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// assignments returns the value of agentops_agentsession_assignments_total for
// the given label values and whether such a series exists.
func assignments(t *testing.T, namespace, pool, result string) (float64, bool) {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	want := map[string]string{"namespace": namespace, "pool": pool, "result": result}
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

func TestRecordSessionAssignment(t *testing.T) {
	const ns, pool = "metrics-test", "pool-a"

	RecordSessionAssignment(ns, pool, AssignmentBound)
	RecordSessionAssignment(ns, pool, AssignmentBound)
	RecordSessionAssignment(ns, pool, AssignmentUnschedulable)
	RecordSessionAssignment(ns, DirectPool, AssignmentBound)

	if got, ok := assignments(t, ns, pool, AssignmentBound); !ok || got != 2 {
		t.Errorf("bound for %s/%s = %v (exists=%v), want 2", ns, pool, got, ok)
	}
	if got, ok := assignments(t, ns, pool, AssignmentUnschedulable); !ok || got != 1 {
		t.Errorf("unschedulable for %s/%s = %v (exists=%v), want 1", ns, pool, got, ok)
	}
	if got, ok := assignments(t, ns, DirectPool, AssignmentBound); !ok || got != 1 {
		t.Errorf("bound for direct sessions = %v (exists=%v), want 1", got, ok)
	}
}

func TestDeleteAgentPoolDropsAssignmentSeries(t *testing.T) {
	const ns, pool = "metrics-test-delete", "pool-b"

	RecordSessionAssignment(ns, pool, AssignmentBound)
	RecordSessionAssignment(ns, DirectPool, AssignmentBound)
	DeleteAgentPool(ns, pool)

	if _, ok := assignments(t, ns, pool, AssignmentBound); ok {
		t.Errorf("series for deleted pool %s/%s still exported", ns, pool)
	}
	if _, ok := assignments(t, ns, DirectPool, AssignmentBound); !ok {
		t.Errorf("deleting pool %s/%s removed the direct-session series", ns, pool)
	}
}
