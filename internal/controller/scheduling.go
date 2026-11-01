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
	"sort"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// activeSessionsByAgent counts Bound sessions per assigned AgentService name.
func activeSessionsByAgent(sessions []agentsv1alpha1.AgentSession) map[string]int32 {
	out := map[string]int32{}
	for _, s := range sessions {
		if s.Status.Phase == agentsv1alpha1.AgentSessionBound && s.Status.AssignedAgent != "" {
			out[s.Status.AssignedAgent]++
		}
	}
	return out
}

// pickAgent selects the member with the lowest load ratio that still has free
// capacity. Members without ready replicas are skipped. Ties are broken by
// name for deterministic placement. It returns "" when the pool is full.
func pickAgent(members []agentsv1alpha1.AgentService, active map[string]int32, perReplica int32) string {
	type cand struct {
		name       string
		load, capa int32
	}
	cands := make([]cand, 0, len(members))
	for _, m := range members {
		capa := m.Status.ReadyReplicas * perReplica
		if capa <= 0 || active[m.Name] >= capa {
			continue
		}
		cands = append(cands, cand{m.Name, active[m.Name], capa})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		// Compare load/capa ratios without floating point: a.load*b.capa < b.load*a.capa.
		li := int64(cands[i].load) * int64(cands[j].capa)
		lj := int64(cands[j].load) * int64(cands[i].capa)
		if li != lj {
			return li < lj
		}
		return cands[i].name < cands[j].name
	})
	return cands[0].name
}
