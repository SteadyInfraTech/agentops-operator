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
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// Field index keys used to look up related objects efficiently from the cache.
const (
	indexServiceRuntimeRef = ".spec.runtimeRef.name"
	indexSessionPoolRef    = ".spec.poolRef.name"
	// indexSessionAssignedAgent finds the sessions bound to an AgentService.
	indexSessionAssignedAgent = ".status.assignedAgent"
)

// SetupIndexes registers the field indexes required by the AgentOps controllers.
// It must be called once before the controllers are started.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &agentsv1alpha1.AgentService{}, indexServiceRuntimeRef, func(o client.Object) []string {
		if ref := o.(*agentsv1alpha1.AgentService).Spec.RuntimeRef; ref != nil {
			return []string{ref.Name}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &agentsv1alpha1.AgentSession{}, indexSessionPoolRef, func(o client.Object) []string {
		if ref := o.(*agentsv1alpha1.AgentSession).Spec.PoolRef; ref != nil {
			return []string{ref.Name}
		}
		return nil
	}); err != nil {
		return err
	}
	return idx.IndexField(ctx, &agentsv1alpha1.AgentSession{}, indexSessionAssignedAgent, func(o client.Object) []string {
		if name := o.(*agentsv1alpha1.AgentSession).Status.AssignedAgent; name != "" {
			return []string{name}
		}
		return nil
	})
}
