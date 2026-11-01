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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

// activeSessionsGauge reads agentops_agentservice_active_sessions for one
// AgentService from the registry the manager serves, and reports whether the
// series exists.
func activeSessionsGauge(ns, name string) (float64, bool) {
	mfs, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, mf := range mfs {
		if mf.GetName() != "agentops_agentservice_active_sessions" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] == ns && labels["name"] == name {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

var _ = Describe("AgentService active-sessions metric", func() {
	It("follows sessions as they bind and finish, and disappears with the AgentService", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: ns},
			Spec:       agentsv1alpha1.AgentServiceSpec{Image: "ghcr.io/example/agent:1.0"},
		})).To(Succeed())
		markDeploymentReady(ns, "a1", 1)

		gauge := func() float64 {
			v, _ := activeSessionsGauge(ns, "a1")
			return v
		}

		By("starting at zero")
		Eventually(gauge, timeout, interval).Should(BeZero())

		By("counting sessions once they are bound")
		for _, name := range []string{"s1", "s2"} {
			Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: agentsv1alpha1.AgentSessionSpec{
					AgentServiceRef: &agentsv1alpha1.LocalObjectReference{Name: "a1"},
				},
			})).To(Succeed())
		}
		Eventually(gauge, timeout, interval).Should(Equal(2.0))

		By("releasing a session when it completes")
		Eventually(func() error {
			s := getSession(ns, "s1")()
			s.Spec.Completed = true
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())
		Eventually(gauge, timeout, interval).Should(Equal(1.0))

		By("dropping the series when the AgentService is deleted")
		Expect(k8sClient.Delete(ctx, getService(ns, "a1")())).To(Succeed())
		Eventually(func() bool {
			_, ok := activeSessionsGauge(ns, "a1")
			return ok
		}, timeout, interval).Should(BeFalse())
	})
})

func TestServicesForSession(t *testing.T) {
	sess := func(assigned string) *agentsv1alpha1.AgentSession {
		s := &agentsv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
		s.Status.AssignedAgent = assigned
		return s
	}

	if got := servicesForSession(context.Background(), sess("")); len(got) != 0 {
		t.Errorf("unassigned session enqueued %v, want nothing", got)
	}

	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a1"}}
	got := servicesForSession(context.Background(), sess("a1"))
	if len(got) != 1 || got[0] != want {
		t.Errorf("assigned session enqueued %v, want [%v]", got, want)
	}

	if got := servicesForSession(context.Background(), &agentsv1alpha1.AgentService{}); len(got) != 0 {
		t.Errorf("non-session object enqueued %v, want nothing", got)
	}
}
