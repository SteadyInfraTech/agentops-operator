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

package agent

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// SessionModel returns how svc holds and serves sessions, applying the same
// defaults as the CRD. The defaults are also applied here because the CRD can
// only default fields inside a `sessions` object that was actually set.
func SessionModel(svc *agentsv1alpha1.AgentService) (agentsv1alpha1.SessionState, agentsv1alpha1.SessionTransport) {
	state, transport := agentsv1alpha1.SessionStateful, agentsv1alpha1.TransportRequest
	if s := svc.Spec.Sessions; s != nil {
		if s.State != "" {
			state = s.State
		}
		if s.Transport != "" {
			transport = s.Transport
		}
	}
	return state, transport
}

// DrainSafeCondition reports whether pods of svc can be removed (scale-in,
// rollout, node drain) without cutting live sessions.
//
// The condition is True only with an explicit guarantee. The only mechanism
// that could provide one is the opt-in drain contract, which does not exist
// yet, so today it is always False and the reason says why. Once draining can
// be enabled (docs/design/session-draining.md, step 3) this is where it turns
// True for the combinations the drain covers.
func DrainSafeCondition(svc *agentsv1alpha1.AgentService) metav1.Condition {
	state, transport := SessionModel(svc)

	reason, msg := agentsv1alpha1.ReasonBestEffortOnly,
		"Stateless agent reached by independent requests: no session state is lost, "+
			"but an in-flight request can be interrupted."
	switch {
	case state == agentsv1alpha1.SessionStateful && transport == agentsv1alpha1.TransportRequest:
		reason = agentsv1alpha1.ReasonRequestTransportWithoutRouting
		msg = "Stateful agent reached by independent requests: without session-aware routing the " +
			"requests of one session can reach different pods, so pods cannot be removed safely."
	case transport == agentsv1alpha1.TransportConnection:
		reason = agentsv1alpha1.ReasonDrainNotEnabled
		msg = "Sessions are long-lived connections and removing a pod cuts them. Draining is not enabled."
	}

	return metav1.Condition{
		Type:               agentsv1alpha1.ConditionDrainSafe,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: svc.Generation,
	}
}
