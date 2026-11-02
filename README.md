# AgentOps Operator

A Kubernetes operator for running **AI agents as microservices**. Built with Go and
[Kubebuilder](https://book.kubebuilder.io) (controller-runtime v0.20, Kubernetes 1.32 APIs).
**Requires Kubernetes 1.30 or newer** (generated pods use the native `preStop` sleep
action). That means the API server **and** the kubelet of every node that runs agent pods,
with the `PodLifecycleSleepAction` feature gate not disabled (it is on by default as beta in
1.30 to 1.33 and GA in 1.34). If a component cannot honor it, the 5 s delay is skipped and
pods terminate as they did before; nothing breaks.

You describe an agent declaratively — image or runtime, model, system prompt, tools,
scaling policy — and the operator turns it into a hardened `Deployment`, a `Service`,
an optional `HorizontalPodAutoscaler`, health probes and Prometheus scrape config.
Pools and sessions sit on top of that for long-running, stateful agent workloads.

> Status: **MVP / `v1alpha1`**. APIs may change.

## Custom resources

| Kind | Short | Purpose |
|------|-------|---------|
| **`AgentService`** | `agent`, `agsvc` | **Primary resource.** One agent microservice → Deployment + Service + HPA. |
| `AgentRuntime` | `art` | Reusable runtime template (framework image, port, probes, resources, env). AgentServices reference it via `spec.runtimeRef` and override any field. |
| `AgentPool` | `apool` | Label-selected group of AgentServices with shared session capacity (`readyReplicas × maxSessionsPerReplica`). |
| `AgentSession` | `asess` | A long-running session scheduled onto the least-loaded agent in a pool (or pinned to one AgentService), with expiry and TTL cleanup. |

```
AgentRuntime ──(runtimeRef)──▶ AgentService ──owns──▶ Deployment / Service / HPA
                                    ▲
                     (label selector)│
                                AgentPool ◀──(poolRef)── AgentSession ──(binds to)──▶ AgentService
```

### AgentService

```yaml
apiVersion: agents.agentops.io/v1alpha1
kind: AgentService
metadata:
  name: support-agent
  labels: { agentops.io/pool: support }
spec:
  runtimeRef: { name: echo-runtime }      # or: image: ghcr.io/acme/agent:1.2
  model:
    provider: openai
    name: gpt-4o-mini
    apiKeySecretRef: { name: llm-credentials, key: apiKey }
  systemPrompt: You are a helpful customer support agent.
  tools: [search, ticketing]
  autoscaling: { enabled: true, minReplicas: 1, maxReplicas: 5, targetCPUUtilizationPercentage: 70 }
  # probes:  { livenessPath: /healthz, readinessPath: /readyz, startupFailureThreshold: 60 }
  # metrics: { enabled: true, port: 9100, path: /metrics }
  # sessions: { state: Stateful, transport: Request }   # both are the defaults
```

```console
$ kubectl get agents
NAME            PHASE     READY   REPLICAS   AGE
support-agent   Running   1       1          2m
```

Phases: `Pending` (e.g. runtime missing) → `Progressing` → `Running`, or `Degraded`
(rollout stalled) / `Failed` (no image). Standard `Ready`/`Progressing`/`Degraded`
conditions are also set, and `status.endpoint` holds the in-cluster URL.

`spec.sessions` declares how the agent holds sessions: `state` is `Stateful` (default, the
agent may keep session state in memory) or `Stateless`; `transport` is `Request` (default,
independent requests) or `Connection` (WebSocket, SSE, gRPC stream). It drives the
`DrainSafe` condition, which says whether pods can be removed (scale-in, rollout, node drain)
without cutting live sessions. It is `True` only with an explicit guarantee, and no such
mechanism exists yet, so it is currently always `False` with a reason: `BestEffortOnly`
(Stateless over Request), `DrainNotEnabled` (Connection) or `RequestTransportWithoutRouting`
(Stateful over Request, which needs session-aware routing). See
[the design note](docs/design/session-draining.md).

The `/scale` subresource is enabled, so `kubectl scale agent/support-agent --replicas=3` works when autoscaling is off.

### AgentSession scheduling

A session in `Pending` is bound to the pool member with the lowest `active/capacity`
ratio that still has free capacity (ties broken by name). It stays `Pending` while the
pool is full and is retried on AgentService changes and every 15s. Lifecycle:

`Pending → Bound → Completed` (client sets `spec.completed: true`) | `Expired` (`maxDurationSeconds`) | `Failed` (assigned agent deleted).
With `ttlSecondsAfterFinished` set, terminal sessions are garbage-collected.

## Agent container contract

The operator injects configuration and expects these endpoints (paths configurable):

| | |
|---|---|
| `GET /healthz` | liveness + startup probe |
| `GET /readyz` | readiness probe (return 503 while warming up / draining) |
| `GET /metrics` | Prometheus metrics (pods get `prometheus.io/scrape|port|path` annotations) |

Environment: `AGENT_NAME`, `AGENT_NAMESPACE`, `AGENT_MODEL_PROVIDER`, `AGENT_MODEL_NAME`,
`AGENT_MODEL_ENDPOINT`, `AGENT_MODEL_API_KEY` (from Secret), `AGENT_SYSTEM_PROMPT`,
`AGENT_TOOLS` (comma-separated), plus any `env`/`envFrom` you specify.

The container must listen on a port named `http` (default 8080), run as non-root,
and tolerate a read-only root filesystem (`/tmp` is an `emptyDir`).
[`examples/echo-agent`](examples/echo-agent) is a minimal reference implementation.

## Rollouts and termination

The operator assumes agents know nothing about AgentOps, so these defaults need no agent
changes:

- **Rollouts never lower capacity.** The Deployment uses `maxUnavailable: 0` (with
  `maxSurge: 25%`): a replacement pod is started before an old one is retired.
- **Short `preStop` sleep (5 s).** After a pod is marked terminating, the container keeps
  running briefly so its removal from the Service endpoints can propagate before it stops
  accepting connections.
- **The termination grace period is Kubernetes' default (30 s)**, which includes the sleep.

What this does **not** do: it does not keep a session alive while its pod is removed. An
agent that handles `SIGTERM` finishes what it can within the grace period; one that does
not is stopped at the end of it. Scale-in and node drains can still cut live sessions.
Making that safe is designed in
[docs/design/session-draining.md](docs/design/session-draining.md) and is not implemented yet.

## Security defaults

Every generated pod gets:
- `runAsNonRoot`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem`,
  `allowPrivilegeEscalation: false`, all capabilities dropped;
- `automountServiceAccountToken: false` unless `spec.serviceAccountName` is set;
- model API keys only via `secretKeyRef` — never inlined in the CR or Deployment.

The operator's own RBAC is generated from `+kubebuilder:rbac` markers and limited to
the resources it manages. It has **no access to Secrets**; it only references them by name.
Its metrics endpoint is served over HTTPS with authn/authz (Kubebuilder default).

## Observability

The manager exposes controller-runtime metrics plus:

| Metric | Labels |
|---|---|
| `agentops_agentservice_replicas` | `namespace`, `name`, `state` = desired \| current \| ready |
| `agentops_agentservice_phase` | `namespace`, `name`, `phase` (1 = current) |
| `agentops_agentservice_active_sessions` | `namespace`, `name` (Bound sessions assigned to the agent; a scaling signal) |
| `agentops_agentpool_sessions` | `namespace`, `name`, `kind` = active \| capacity |
| `agentops_agentsession_transitions_total` | `namespace`, `phase` |
| `agentops_agentsession_assignments_total` | `namespace`, `target` = pool \| agent, `result` = bound \| unschedulable |

Enable `config/prometheus` in `config/default/kustomization.yaml` for a `ServiceMonitor`
(requires prometheus-operator).

## Getting started

Prerequisites: Go 1.24+, Docker, kubectl, a cluster (e.g. `kind`), and
[metrics-server](https://github.com/kubernetes-sigs/metrics-server) for the HPA.

```sh
# Build and load images (kind example)
make docker-build IMG=agentops-operator:dev
make docker-build-agent AGENT_IMG=ghcr.io/steadyinfratech/echo-agent:latest
kind load docker-image agentops-operator:dev ghcr.io/steadyinfratech/echo-agent:latest

# Install CRDs and deploy the operator
make install
make deploy IMG=agentops-operator:dev

# Create a runtime, two agents, a pool and a session
kubectl apply -k config/samples/
kubectl get art,agents,apool,asess

# Talk to an agent
kubectl port-forward svc/support-agent 8080 &
curl -XPOST localhost:8080/v1/chat -d '{"message":"hello"}'
```

Uninstall: `kubectl delete -k config/samples/ && make undeploy && make uninstall`.

## Development

```sh
make generate manifests   # regenerate deepcopy, CRDs and RBAC after editing api/ or markers
make test                 # unit tests + envtest integration tests (real kube-apiserver/etcd)
make lint                 # golangci-lint
make run                  # run the controller against your current kubeconfig
make test-e2e             # Kind-based end-to-end tests
```

Layout:

```
api/v1alpha1/          CRD Go types + validation markers (CEL rules for cross-field checks)
internal/agent/        pure functions: merge runtime+service, build Deployment/Service/HPA
internal/controller/   reconcilers for the four kinds, field indexes, session scheduling
internal/metrics/      custom Prometheus collectors
examples/echo-agent/   reference agent implementing the container contract
config/                Kustomize manifests (CRDs, RBAC, manager, samples)
```

## Known limitations / roadmap

- `AgentSession` is a scheduling/bookkeeping primitive: it records the assigned agent and
  endpoint but does not route traffic. Sticky routing (gateway / header-based) is future work.
- Session placement uses an uncached read to avoid over-commit, but the controller runs a
  single worker; if `MaxConcurrentReconciles` is raised, placement needs optimistic locking
  (e.g. a per-pool lease or capacity reservation).
- A pool member deleted while it holds sessions fails those sessions rather than rescheduling.
- The generated HPA scales on CPU/memory only. `agentops_agentservice_active_sessions` is
  available as a better signal (e.g. a KEDA `prometheus` trigger) but is not wired in yet.
  For Stateful agents (the default) the HPA scales in slowly: a 15 min stabilization window
  and at most one pod per 5 min. This reduces how often sessions are cut; it does not choose
  which pod is removed. The policy does not limit one case: lowering `maxReplicas` below the
  current replica count scales straight to the new maximum. `sessions.state: Stateless`
  keeps the Kubernetes defaults.
- Scale-in, rollouts and node drains can terminate a pod that is serving a session: the
  operator sets no termination grace period or drain hook. See
  [docs/design/session-draining.md](docs/design/session-draining.md).
- No admission webhooks yet; validation is via OpenAPI schema + CEL.
- Changing an AgentService's `runtimeRef` refreshes the new runtime's `status.agentServices`
  immediately; the previous runtime's count updates on its next reconcile.

## License

Apache 2.0
