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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

var _ = Describe("AgentService DrainSafe condition", func() {
	drainSafeReason := func(ns, name string) func() string {
		return func() string {
			c := meta.FindStatusCondition(getService(ns, name)().Status.Conditions, agentsv1alpha1.ConditionDrainSafe)
			if c == nil || c.Status != metav1.ConditionFalse {
				return ""
			}
			return c.Reason
		}
	}

	It("reports why pods cannot be removed safely, from the declared session model", func() {
		ns := newNamespace()
		create := func(name string, s *agentsv1alpha1.SessionsSpec) {
			Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       agentsv1alpha1.AgentServiceSpec{Image: "ghcr.io/example/agent:1.0", Sessions: s},
			})).To(Succeed())
		}
		create("default", nil)
		create("stateless", &agentsv1alpha1.SessionsSpec{State: agentsv1alpha1.SessionStateless})
		create("conn", &agentsv1alpha1.SessionsSpec{Transport: agentsv1alpha1.TransportConnection})

		Eventually(drainSafeReason(ns, "default"), timeout, interval).
			Should(Equal(agentsv1alpha1.ReasonRequestTransportWithoutRouting))
		Eventually(drainSafeReason(ns, "stateless"), timeout, interval).
			Should(Equal(agentsv1alpha1.ReasonBestEffortOnly))
		Eventually(drainSafeReason(ns, "conn"), timeout, interval).
			Should(Equal(agentsv1alpha1.ReasonDrainNotEnabled))
	})

	It("defaults an empty sessions object and rejects unknown values", func() {
		ns := newNamespace()
		svc := &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				Image:    "ghcr.io/example/agent:1.0",
				Sessions: &agentsv1alpha1.SessionsSpec{},
			},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		Expect(svc.Spec.Sessions.State).To(Equal(agentsv1alpha1.SessionStateful))
		Expect(svc.Spec.Sessions.Transport).To(Equal(agentsv1alpha1.TransportRequest))

		bad := svc.DeepCopy()
		bad.ObjectMeta = metav1.ObjectMeta{Name: "bad", Namespace: ns}
		bad.Spec.Sessions = &agentsv1alpha1.SessionsSpec{State: "Sometimes"}
		err := k8sClient.Create(ctx, bad)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})
})
