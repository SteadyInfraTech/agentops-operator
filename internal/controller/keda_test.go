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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
	"github.com/SteadyInfraTech/agentops-operator/internal/agent"
)

const testPrometheusAddress = "http://prometheus.test:9090"

var _ = Describe("AgentService scaling on sessions with KEDA", func() {
	scaledObject := func(ns, name string) func() error {
		return func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, agent.NewScaledObject(
				&agentsv1alpha1.AgentService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}))
		}
	}
	gone := func(get func() error) func() bool {
		return func() bool { return apierrors.IsNotFound(get()) }
	}
	hpa := func(ns, name string) func() error {
		return func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &autoscalingv2.HorizontalPodAutoscaler{})
		}
	}

	It("uses a ScaledObject instead of an HPA, and switches back when the target is removed", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				Image: "ghcr.io/example/agent:1.0",
				Autoscaling: &agentsv1alpha1.AutoscalingSpec{
					Enabled: true, MinReplicas: 1, MaxReplicas: 4, TargetSessionsPerPod: ptr.To[int32](5),
				},
			},
		})).To(Succeed())

		By("creating the ScaledObject")
		Eventually(scaledObject(ns, "a1"), timeout, interval).Should(Succeed())
		so := agent.NewScaledObject(&agentsv1alpha1.AgentService{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: ns}})
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "a1"}, so)).To(Succeed())

		minReplicas, _, _ := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
		maxReplicas, _, _ := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount")
		Expect(minReplicas).To(Equal(int64(1)))
		Expect(maxReplicas).To(Equal(int64(4)))
		target, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "name")
		Expect(target).To(Equal("a1"))

		triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
		Expect(triggers).NotTo(BeEmpty())
		sessions := triggers[0].(map[string]interface{})
		Expect(sessions["type"]).To(Equal("prometheus"))
		Expect(sessions["metadata"]).To(And(
			HaveKeyWithValue("serverAddress", testPrometheusAddress),
			HaveKeyWithValue("threshold", "5"),
			HaveKeyWithValue("query", `sum(agentops_agentservice_active_sessions{namespace="`+ns+`",name="a1"})`),
		))
		Expect(metav1.IsControlledBy(so, getService(ns, "a1")())).To(BeTrue())

		By("never keeping an HPA next to it")
		Eventually(gone(hpa(ns, "a1")), timeout, interval).Should(BeTrue())

		By("falling back to an HPA when the target is removed")
		Eventually(func() error {
			s := getService(ns, "a1")()
			s.Spec.Autoscaling.TargetSessionsPerPod = nil
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())
		Eventually(hpa(ns, "a1"), timeout, interval).Should(Succeed())
		Eventually(gone(scaledObject(ns, "a1")), timeout, interval).Should(BeTrue())
	})

	It("does not create a ScaledObject while someone else's HPA targets the Deployment", func() {
		ns := newNamespace()
		theirs := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "their-hpa", Namespace: ns},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "a3"},
				MinReplicas:    ptr.To[int32](1),
				MaxReplicas:    2,
			},
		}
		Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "a3", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				Image: "ghcr.io/example/agent:1.0",
				Autoscaling: &agentsv1alpha1.AutoscalingSpec{
					Enabled: true, MinReplicas: 1, MaxReplicas: 4, TargetSessionsPerPod: ptr.To[int32](5),
				},
			},
		})).To(Succeed())

		By("leaving their HPA alone and creating no ScaledObject")
		Consistently(gone(scaledObject(ns, "a3")), "3s", interval).Should(BeTrue())
		Expect(hpa(ns, "their-hpa")()).To(Succeed())

		By("creating the ScaledObject once their HPA is gone")
		Expect(k8sClient.Delete(ctx, theirs)).To(Succeed())
		Eventually(scaledObject(ns, "a3"), timeout, interval).Should(Succeed())
	})

	It("keeps the HPA and leaves a ScaledObject alone when no target is set", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, &agentsv1alpha1.AgentService{
			ObjectMeta: metav1.ObjectMeta{Name: "a2", Namespace: ns},
			Spec: agentsv1alpha1.AgentServiceSpec{
				Image:       "ghcr.io/example/agent:1.0",
				Autoscaling: &agentsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 3},
			},
		})).To(Succeed())
		Eventually(hpa(ns, "a2"), timeout, interval).Should(Succeed())
		Consistently(gone(scaledObject(ns, "a2")), "2s", interval).Should(BeTrue())
	})
})

func TestIsForeignHPA(t *testing.T) {
	svc := &agentsv1alpha1.AgentService{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "svc-uid"}}
	hpa := func(kind, target string, owners ...metav1.OwnerReference) *autoscalingv2.HorizontalPodAutoscaler {
		return &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns", OwnerReferences: owners},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: target},
			},
		}
	}
	controller := true
	ours := metav1.OwnerReference{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "AgentService", Name: "a", UID: "svc-uid", Controller: &controller}
	keda := metav1.OwnerReference{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Name: "a", UID: "so-uid", Controller: &controller}

	tests := []struct {
		name string
		hpa  *autoscalingv2.HorizontalPodAutoscaler
		want bool
	}{
		{"user HPA on the Deployment", hpa("Deployment", "a"), true},
		{"HPA on another Deployment", hpa("Deployment", "b"), false},
		{"HPA on another kind", hpa("StatefulSet", "a"), false},
		{"HPA owned by the operator", hpa("Deployment", "a", ours), false},
		{"HPA created by KEDA", hpa("Deployment", "a", keda), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForeignHPA(tt.hpa, svc); got != tt.want {
				t.Errorf("isForeignHPA = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServicesForHPA(t *testing.T) {
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}
	h.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "a"}
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}}
	if got := servicesForHPA(context.Background(), h); len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want [%v]", got, want)
	}
	h.Spec.ScaleTargetRef.Kind = "StatefulSet"
	if got := servicesForHPA(context.Background(), h); len(got) != 0 {
		t.Errorf("non-Deployment target enqueued %v", got)
	}
}
