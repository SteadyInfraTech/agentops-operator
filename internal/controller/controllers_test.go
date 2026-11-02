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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

const (
	timeout  = 10 * time.Second
	interval = 100 * time.Millisecond
)

var nsCounter int

func newNamespace() string {
	nsCounter++
	name := fmt.Sprintf("agentops-test-%d", nsCounter)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

// markDeploymentReady simulates the kube-controller-manager (absent in envtest).
func markDeploymentReady(ns, name string, replicas int32) {
	Eventually(func() error {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, dep); err != nil {
			return err
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas = replicas
		dep.Status.UpdatedReplicas = replicas
		dep.Status.ReadyReplicas = replicas
		dep.Status.AvailableReplicas = replicas
		return k8sClient.Status().Update(ctx, dep)
	}, timeout, interval).Should(Succeed())
}

func getService(ns, name string) func() *agentsv1alpha1.AgentService {
	return func() *agentsv1alpha1.AgentService {
		s := &agentsv1alpha1.AgentService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)).To(Succeed())
		return s
	}
}

func getSession(ns, name string) func() *agentsv1alpha1.AgentSession {
	return func() *agentsv1alpha1.AgentSession {
		s := &agentsv1alpha1.AgentSession{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)).To(Succeed())
		return s
	}
}

var _ = Describe("AgentService", func() {
	It("creates a hardened Deployment, Service and HPA and reports Running", func() {
		ns := newNamespace()
		svc := &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				Image: "ghcr.io/example/agent:1.0",
				Model: &agentsv1alpha1.ModelSpec{
					Provider: "openai", Name: "gpt-4o",
					APIKeySecretRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "llm"}, Key: "apiKey",
					},
				},
				Autoscaling: &agentsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 4},
			},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())

		dep := &appsv1.Deployment{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "support"}, dep)
		}, timeout, interval).Should(Succeed())

		Expect(*dep.Spec.Replicas).To(Equal(int32(2)), "seeded from HPA minReplicas")
		Expect(metav1.IsControlledBy(dep, getService(ns, "support")())).To(BeTrue())
		c := dep.Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal("ghcr.io/example/agent:1.0"))
		Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))
		Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
		Expect(c.StartupProbe).NotTo(BeNil())
		Expect(*c.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(*dep.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*dep.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue("prometheus.io/scrape", "true"))
		Expect(c.Env).To(ContainElement(HaveField("Name", "AGENT_MODEL_API_KEY")))
		for _, e := range c.Env {
			if e.Name == "AGENT_MODEL_API_KEY" {
				Expect(e.Value).To(BeEmpty(), "API key must come from a secretKeyRef")
				Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("llm"))
			}
		}

		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "support"}, &corev1.Service{})
		}, timeout, interval).Should(Succeed())
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "support"}, hpa)
		}, timeout, interval).Should(Succeed())
		Expect(hpa.Spec.MaxReplicas).To(Equal(int32(4)))
		Expect(hpa.Spec.Behavior).NotTo(BeNil(), "Stateful is the default")
		Expect(hpa.Spec.Behavior.ScaleDown).NotTo(BeNil())

		Eventually(func() agentsv1alpha1.AgentServicePhase { return getService(ns, "support")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentServiceProgressing))

		markDeploymentReady(ns, "support", 2)
		Eventually(func() agentsv1alpha1.AgentServicePhase { return getService(ns, "support")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentServiceRunning))
		Expect(getService(ns, "support")().Status.Endpoint).To(Equal(fmt.Sprintf("http://support.%s.svc:8080", ns)))

		By("disabling autoscaling removes the HPA and applies static replicas")
		Eventually(func() error {
			s := getService(ns, "support")()
			s.Spec.Autoscaling.Enabled = false
			s.Spec.Replicas = ptr.To[int32](3)
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "support"}, &autoscalingv2.HorizontalPodAutoscaler{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
		Eventually(func() int32 {
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "support"}, d)).To(Succeed())
			return *d.Spec.Replicas
		}, timeout, interval).Should(Equal(int32(3)))
	})

	It("waits for a missing AgentRuntime and picks up its defaults once created", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				RuntimeRef: &agentsv1alpha1.LocalObjectReference{Name: "langgraph"},
			},
		})).To(Succeed())

		Eventually(func() agentsv1alpha1.AgentServicePhase { return getService(ns, "researcher")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentServicePending))

		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentRuntime{
			ObjectMeta: metav1.ObjectMeta{Name: "langgraph", Namespace: ns},
			Spec:       agentsv1alpha1.AgentRuntimeSpec{Image: "ghcr.io/example/langgraph:0.2", Port: 9000},
		})).To(Succeed())

		dep := &appsv1.Deployment{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "researcher"}, dep)
		}, timeout, interval).Should(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/example/langgraph:0.2"))
		Expect(dep.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(9000)))

		Eventually(func() int32 {
			rt := &agentsv1alpha1.AgentRuntime{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "langgraph"}, rt)).To(Succeed())
			return rt.Status.AgentServices
		}, timeout, interval).Should(Equal(int32(1)))
	})

	It("rejects an AgentService without image or runtimeRef", func() {
		ns := newNamespace()
		err := k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})
})

var _ = Describe("AgentPool and AgentSession", func() {
	It("aggregates members, schedules sessions within capacity and completes them", func() {
		ns := newNamespace()
		for _, name := range []string{"a1", "a2"} {
			Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"pool": "chat"}},
				Spec:       agentsv1alpha1.AgentServiceSpec{Image: "ghcr.io/example/agent:1.0"},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: ns},
			Spec: agentsv1alpha1.AgentPoolSpec{
				Selector:              metav1.LabelSelector{MatchLabels: map[string]string{"pool": "chat"}},
				MaxSessionsPerReplica: 1,
			},
		})).To(Succeed())

		newSession := func(name string) {
			Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       agentsv1alpha1.AgentSessionSpec{PoolRef: &agentsv1alpha1.LocalObjectReference{Name: "chat"}},
			})).To(Succeed())
		}

		By("staying Pending while no agent is ready")
		newSession("s1")
		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s1")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionPending))

		By("binding once agents become ready")
		markDeploymentReady(ns, "a1", 1)
		markDeploymentReady(ns, "a2", 1)
		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s1")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionBound))

		newSession("s2")
		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s2")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionBound))
		Expect(getSession(ns, "s2")().Status.AssignedAgent).NotTo(Equal(getSession(ns, "s1")().Status.AssignedAgent),
			"least-loaded scheduling spreads sessions")

		By("leaving the third session Pending when the pool is full")
		newSession("s3")
		Consistently(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s3")().Status.Phase },
			2*time.Second, interval).ShouldNot(Equal(agentsv1alpha1.AgentSessionBound))

		pool := &agentsv1alpha1.AgentPool{}
		Eventually(func() int32 {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "chat"}, pool)).To(Succeed())
			return pool.Status.ActiveSessions
		}, timeout, interval).Should(Equal(int32(2)))
		Expect(pool.Status.Capacity).To(Equal(int32(2)))
		Expect(pool.Status.ReadyAgents).To(Equal(int32(2)))

		By("completing a session frees capacity for the pending one")
		Eventually(func() error {
			s := getSession(ns, "s1")()
			s.Spec.Completed = true
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())
		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s1")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionCompleted))
		// s3 is retried by the periodic requeue for Pending sessions.
		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "s3")().Status.Phase },
			pendingRequeue+timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionBound))
	})

	It("expires sessions after maxDurationSeconds and deletes them after the TTL", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: ns},
			Spec:       agentsv1alpha1.AgentServiceSpec{Image: "ghcr.io/example/agent:1.0"},
		})).To(Succeed())
		markDeploymentReady(ns, "solo", 1)
		Eventually(func() int32 { return getService(ns, "solo")().Status.ReadyReplicas }, timeout, interval).Should(Equal(int32(1)))

		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "short", Namespace: ns},
			Spec: agentsv1alpha1.AgentSessionSpec{
				AgentServiceRef:         &agentsv1alpha1.LocalObjectReference{Name: "solo"},
				MaxDurationSeconds:      1,
				TTLSecondsAfterFinished: ptr.To[int64](1),
			},
		})).To(Succeed())

		Eventually(func() agentsv1alpha1.AgentSessionPhase { return getSession(ns, "short")().Status.Phase },
			timeout, interval).Should(Equal(agentsv1alpha1.AgentSessionExpired))
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "short"}, &agentsv1alpha1.AgentSession{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})

	It("rejects sessions that set both poolRef and agentServiceRef", func() {
		ns := newNamespace()
		err := k8sClient.Create(ctx, &agentsv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: ns},
			Spec: agentsv1alpha1.AgentSessionSpec{
				PoolRef:         &agentsv1alpha1.LocalObjectReference{Name: "p"},
				AgentServiceRef: &agentsv1alpha1.LocalObjectReference{Name: "a"},
			},
		})
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})
})
