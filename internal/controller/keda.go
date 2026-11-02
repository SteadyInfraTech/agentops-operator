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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/SteadyInfraTech/agentops-operator/internal/agent"
)

// DetectKEDA reports whether the cluster serves the KEDA ScaledObject API. It
// is checked once at startup: installing KEDA later needs an operator restart.
// A missing group is "not available", not an error.
func DetectKEDA(cfg *rest.Config) (bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("creating discovery client: %w", err)
	}
	gv := agent.ScaledObjectGVK.GroupVersion().String()
	list, err := dc.ServerResourcesForGroupVersion(gv)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("discovering %s: %w", gv, err)
	}
	for _, r := range list.APIResources {
		if r.Kind == agent.ScaledObjectGVK.Kind {
			return true, nil
		}
	}
	return false, nil
}
