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

// Package agent contains pure functions that turn AgentOps API objects into
// the Kubernetes workload objects (Deployment, Service, HPA) that run them.
package agent

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/SteadyInfraTech/agentops-operator/api/v1alpha1"
)

const (
	DefaultPort                    int32 = 8080
	DefaultLivenessPath                  = "/healthz"
	DefaultReadinessPath                 = "/readyz"
	DefaultMetricsPath                   = "/metrics"
	DefaultStartupFailureThreshold int32 = 60
)

// Resolved is the effective configuration of an agent after merging the
// optional AgentRuntime defaults with AgentService overrides.
type Resolved struct {
	Image           string
	ImagePullPolicy corev1.PullPolicy
	Command         []string
	Args            []string
	Port            int32
	Env             []corev1.EnvVar
	Resources       corev1.ResourceRequirements
	Probes          agentsv1alpha1.ProbeSpec
	MetricsEnabled  bool
	MetricsPort     int32
	MetricsPath     string
}

// Resolve merges runtime (may be nil) and service into the effective config.
// Service fields win over runtime fields; env vars are merged by name.
func Resolve(svc *agentsv1alpha1.AgentService, rt *agentsv1alpha1.AgentRuntime) Resolved {
	r := Resolved{Port: DefaultPort, ImagePullPolicy: corev1.PullIfNotPresent}
	var rtProbes *agentsv1alpha1.ProbeSpec
	var rtMetrics *agentsv1alpha1.MetricsSpec

	if rt != nil {
		r.Image = rt.Spec.Image
		if rt.Spec.ImagePullPolicy != "" {
			r.ImagePullPolicy = rt.Spec.ImagePullPolicy
		}
		r.Command = rt.Spec.Command
		r.Args = rt.Spec.Args
		if rt.Spec.Port != 0 {
			r.Port = rt.Spec.Port
		}
		r.Env = append(r.Env, rt.Spec.Env...)
		r.Resources = *rt.Spec.Resources.DeepCopy()
		rtProbes, rtMetrics = rt.Spec.Probes, rt.Spec.Metrics
	}

	s := svc.Spec
	if s.Image != "" {
		r.Image = s.Image
	}
	if s.ImagePullPolicy != "" {
		r.ImagePullPolicy = s.ImagePullPolicy
	}
	if len(s.Command) > 0 {
		r.Command = s.Command
	}
	if len(s.Args) > 0 {
		r.Args = s.Args
	}
	if s.Port != 0 {
		r.Port = s.Port
	}
	if s.Resources != nil {
		r.Resources = *s.Resources.DeepCopy()
	}

	r.Env = mergeEnv(r.Env, agentEnv(svc))
	r.Env = mergeEnv(r.Env, s.Env)

	r.Probes = resolveProbes(rtProbes, s.Probes)
	r.MetricsEnabled, r.MetricsPort, r.MetricsPath = resolveMetrics(rtMetrics, s.Metrics, r.Port)
	return r
}

// agentEnv converts the declarative agent config into environment variables.
func agentEnv(svc *agentsv1alpha1.AgentService) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "AGENT_NAME", Value: svc.Name},
		{Name: "AGENT_NAMESPACE", Value: svc.Namespace},
	}
	if m := svc.Spec.Model; m != nil {
		env = append(env,
			corev1.EnvVar{Name: "AGENT_MODEL_PROVIDER", Value: m.Provider},
			corev1.EnvVar{Name: "AGENT_MODEL_NAME", Value: m.Name},
		)
		if m.Endpoint != "" {
			env = append(env, corev1.EnvVar{Name: "AGENT_MODEL_ENDPOINT", Value: m.Endpoint})
		}
		if m.APIKeySecretRef != nil {
			env = append(env, corev1.EnvVar{
				Name:      "AGENT_MODEL_API_KEY",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: m.APIKeySecretRef.DeepCopy()},
			})
		}
	}
	if svc.Spec.SystemPrompt != "" {
		env = append(env, corev1.EnvVar{Name: "AGENT_SYSTEM_PROMPT", Value: svc.Spec.SystemPrompt})
	}
	if len(svc.Spec.Tools) > 0 {
		env = append(env, corev1.EnvVar{Name: "AGENT_TOOLS", Value: strings.Join(svc.Spec.Tools, ",")})
	}
	return env
}

// mergeEnv returns base with overrides applied: same-name entries are replaced
// in place, new entries are appended. Order is deterministic so the generated
// pod template does not churn between reconciles.
func mergeEnv(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(base)+len(overrides))
	idx := make(map[string]int, len(base)+len(overrides))
	for _, e := range append(append([]corev1.EnvVar{}, base...), overrides...) {
		if i, ok := idx[e.Name]; ok {
			out[i] = e
			continue
		}
		idx[e.Name] = len(out)
		out = append(out, e)
	}
	return out
}

func resolveProbes(layers ...*agentsv1alpha1.ProbeSpec) agentsv1alpha1.ProbeSpec {
	p := agentsv1alpha1.ProbeSpec{
		LivenessPath:            DefaultLivenessPath,
		ReadinessPath:           DefaultReadinessPath,
		StartupFailureThreshold: DefaultStartupFailureThreshold,
	}
	for _, l := range layers {
		if l == nil {
			continue
		}
		if l.LivenessPath != "" {
			p.LivenessPath = l.LivenessPath
		}
		if l.ReadinessPath != "" {
			p.ReadinessPath = l.ReadinessPath
		}
		if l.StartupFailureThreshold != 0 {
			p.StartupFailureThreshold = l.StartupFailureThreshold
		}
	}
	return p
}

func resolveMetrics(rt, svc *agentsv1alpha1.MetricsSpec, appPort int32) (bool, int32, string) {
	enabled, port, path := true, appPort, DefaultMetricsPath
	for _, l := range []*agentsv1alpha1.MetricsSpec{rt, svc} {
		if l == nil {
			continue
		}
		if l.Enabled != nil {
			enabled = *l.Enabled
		}
		if l.Port != 0 {
			port = l.Port
		}
		if l.Path != "" {
			path = l.Path
		}
	}
	return enabled, port, path
}
