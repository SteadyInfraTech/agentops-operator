# Design note: draining agents without dropping sessions

Status: **proposal, open decisions below**. Nothing here is implemented.

## Problem

Anything that removes an agent pod can cut off a live `AgentSession`:

| Trigger | Who picks the victim |
|---|---|
| HPA / KEDA scale-in | the ReplicaSet controller (not session-aware) |
| Rolling update of the Deployment | the Deployment controller |
| Node drain / eviction | the scheduler / admin |

Today the operator does nothing about this:

- Generated pods set no `terminationGracePeriodSeconds`, so Kubernetes' default of 30 s
  applies, and there is no `preStop` hook.
- The container contract in the README says `/readyz` returns 503 "while warming up /
  draining", but nothing tells an agent *when* to drain or for how long.
- The operator cannot tell which pod holds which session. A session binds to an
  `AgentService` (`status.assignedAgent`, `status.endpoint` = the Service address), never
  to a pod. The only load figure it has is the per-service count in
  `agentops_agentservice_active_sessions`.
- If a pod holding a session goes away, the session stays `Bound`: the `AgentService`
  still exists, so the operator sees nothing wrong. Clients find out from the lost
  connection.

`AgentSession.spec.maxDurationSeconds` defaults to 3600, which is too long to wait for on
every rollout or node drain. The drain budget is therefore its own setting (below), not
derived from the session lifetime; sessions that outlive it are cut.

## Requirements

The operator must not rely on agents keeping session state in memory, but must also serve
agents that do. Sessions may be one long-lived connection (WebSocket, SSE, gRPC stream) or
many independent requests. Both have to be supported, so both are declared per
`AgentService` rather than assumed.

## Three separate concerns

1. **Stop new work reaching a draining pod.**
2. **Let existing sessions finish**, up to a bounded time.
3. **Prefer killing idle pods** when the choice is ours (scale-in only).

They need different mechanisms, and only the first two are required for correctness.

## Declaring the session model

```yaml
spec:
  sessions:
    state: Stateful          # default. Stateless = no in-memory session state
    transport: Connection    # Connection | Request
  lifecycle:
    drainTimeoutSeconds: 300     # max time a draining agent may keep serving sessions
    shutdownMarginSeconds: 20    # time the agent needs to exit after draining
```

`state` and `transport` are independent questions:

- `state` is whether losing the pod loses data. `Stateful` is the default because it is the
  safe assumption: the operator never treats an agent as freely killable unless the user
  says it is.
- `transport` is whether losing the pod interrupts something in flight. A long-lived
  stream is cut by any pod removal whether or not the agent holds state, so connection
  draining applies to every `Connection` agent, `Stateless` included. For a `Stateless`
  agent it just means clients can reconnect to another pod without losing anything.

| `state` | `transport` | Drain-safe? | How |
|---|---|---|---|
| `Stateless` | `Request` | yes | in-flight requests only: short drain |
| `Stateless` | `Connection` | yes | fail `/readyz`; wait for streams to close; clients reconnect elsewhere with nothing lost |
| `Stateful` | `Connection` | yes | same, but the state lives as long as the connection |
| `Stateful` | `Request` | **not with plain Service routing** | needs session-aware routing (see below) |

Failing `/readyz` removes the pod from the Service endpoints. Established connections keep
flowing in the usual kube-proxy modes while new ones go to other pods, which is why this is
correct for `Connection` and wrong for `Request` (every new request is a new connection).

### Drain timeout and grace period

Users configure what they care about, the drain budget; the operator derives the pod's
`terminationGracePeriodSeconds`:

```
terminationGracePeriodSeconds = drainTimeoutSeconds + shutdownMarginSeconds
preStop wait                  = drainTimeoutSeconds
```

This removes a class of invalid configurations by construction: there is no independent
grace period that could be smaller than the margin, so the wait can never be non-positive.

| Setting | Default | Bounds |
|---|---|---|
| `drainTimeoutSeconds` | `300` for `Stateful` or `Connection`; `10` for `Stateless` + `Request` | 0 to 3600 (proposed cap) |
| `shutdownMarginSeconds` | `20`, matching the echo agent's 20 s shutdown timeout | 1 to 300 (proposed) |

The `Stateless` + `Request` default derives a 30 s grace period, equal to Kubernetes' own
default. An agent that needs longer than the margin to exit must raise it.

Caveat: an agent that ignores `SIGTERM` makes every rollout wait the full derived grace
period before the kubelet sends `SIGKILL`. Handling `SIGTERM` is therefore a hard
requirement of the contract below, not a nicety.

### Why `Stateful` + `Request` needs routing first

With a ClusterIP Service every request may land on a different pod, so in-memory state is
already broken in steady state, before any scale-in. `sessionAffinity: ClientIP` only
partly helps (clients behind one NAT share a pod) and it defeats retries: a retry from the
same client IP returns to the same pod. Draining such sessions safely needs the router to
know which pod owns a session and to stop assigning new sessions to a draining pod. That is
the sticky-routing work the README already lists as future work, and it should come before
drain support for this combination. Until then the operator should report it rather than
pretend: a `DrainSafe=False` condition with reason `RequestTransportWithoutRouting`.

### Routing choice: consistent hashing, with a caveat

Hashing a session identifier (a header or the `AgentSession` name) onto the set of agent
pods is a good trade-off for placement: no lookup table, and most sessions keep their pod
when pods are added or removed.

It is not sufficient for draining on its own. A pure hash does not know a pod is draining;
removing the pod from the ring remaps its sessions, which is exactly the loss we are
avoiding. A draining pod must keep receiving the sessions it already owns while being
excluded for new ones. Proposal: hash for *new* sessions, plus explicit pins for sessions
owned by a draining pod, fed from agent-reported state (step 3). Adding or removing pods
also remaps roughly `1/N` of sessions even without draining, so `Stateful` agents need the
pins for scale-out as well, or must tolerate that loss.

This needs a data-plane component the operator does not have today. Prefer an existing
gateway (Gateway API implementation or Envoy-based) over a custom proxy. The choice of
gateway is open (see decisions).

### Agent-side rejection of new sessions (not a general answer)

A draining agent could keep `/readyz` = 200, keep serving sessions it knows, and answer
requests that start a new session with `503` + `Retry-After`. Note the Service does **not**
retry: kube-proxy balances per connection and ignores HTTP status codes. The client must
retry on a *new* connection; a keep-alive retry, or `sessionAffinity: ClientIP`, goes back
to the same pod. This only helps clients that already retry correctly and does not fix
steady-state routing, so it is a fallback, not the plan.

## Proposal, in order

### Step 1: bounded termination (operator only, no agent change)

Add `lifecycle.drainTimeoutSeconds` and `lifecycle.shutdownMarginSeconds` to
`AgentServiceSpec` / `AgentRuntimeSpec` (service overrides runtime) and set the derived
`terminationGracePeriodSeconds` on the pod template. Also set
`strategy.rollingUpdate.maxUnavailable: 0` so a rollout never lowers capacity below the
desired count. Agents that already handle `SIGTERM` (the echo agent does) benefit
immediately.

### Step 2: scale on sessions, and scale in slowly

Wire `agentops_agentservice_active_sessions` into scaling (KEDA `prometheus` trigger or HPA
external metric). For `Stateful` agents use a long scale-down stabilization window by
default; `Stateless` agents may use the normal one. This reduces how often scale-in
happens but does **not** choose which pod dies, so it is not a safety mechanism by itself.

### Step 3: drain contract

Extend the container contract:

| | |
|---|---|
| `SIGTERM` | **must** start draining and exit `0` once idle or within `shutdownMarginSeconds`. A grace period alone does not make an agent finish work |
| `GET /drain?wait=<seconds>` on the `admin` port | starts draining (idempotent) and returns when the agent is idle or `wait` elapses. Used by the `preStop` hook |
| `GET /metrics` | adds `agent_active_sessions` (gauge) so the operator and KEDA can see per-pod load |

While draining an agent rejects new sessions and, for `transport: Connection`, fails
`/readyz`.

**`/drain` must not be reachable from outside the pod network.** The generated Service
forwards the agent port and may be `NodePort` or `LoadBalancer`, so an unauthenticated
`/drain` on that port would let any caller drain agents. Instead the operator adds a
separate container port named `admin` (default `9101`) that is **never** included in the
generated Service. Service ports are built from an explicit list, so this is a matter of
not adding it. The kubelet's `httpGet` hook reaches the pod IP directly. Residual risk:
other pods in the cluster can still reach the pod IP, so ship a `NetworkPolicy` that admits
only the node and namespace that need it, and document that agents must bind `/drain` to
the admin port only.

The operator adds a `preStop` hook that **triggers** draining and then waits for idle. A
Kubernetes `httpGet` hook can only issue `GET`, but it does block termination until the
handler returns, which is why `/drain` is a blocking `GET` and needs no extra binary in the
image. The grace-period clock starts *before* `preStop` runs and `SIGTERM` is sent only
after it finishes, so the wait must end before the grace period does. That is what the
derived `terminationGracePeriodSeconds = drainTimeoutSeconds + shutdownMarginSeconds`
guarantees: the wait is `drainTimeoutSeconds`, leaving the full margin for `SIGTERM`.
Update the README line about `/readyz` to say that failing readiness is the drain signal
for `transport: Connection` only.

### Step 4 (optional, best effort): pod-deletion-cost

If agents report per-pod load, the operator can annotate pods with
`controller.kubernetes.io/pod-deletion-cost` so the ReplicaSet controller prefers idle
pods on scale-in. Caveats: it is only a preference, it costs an API write per pod per
change (rate-limit it), it needs `pods` get/list/patch RBAC, and it is useless without the
per-pod load from step 3.

## Open decisions

Settled: agents may or may not keep state (default `Stateful`, `Stateless` opts out); both
transports are supported and declared per `AgentService`; the drain budget is its own
configurable setting with defaults above, not the session maximum; idle is agent-reported.

1. **Routing for `Stateful` + `Request`.** Consistent hashing with pins for draining pods
   is the leading option. Which gateway implementation, and who owns session-to-pod pins?
2. **What happens to sessions cut by a drain timeout?** They stay `Bound` today. Should
   agents report session end so the operator can mark them `Failed` with a reason such as
   `DrainTimeout`?
3. **Are the proposed caps right?** `drainTimeoutSeconds` max 3600 and
   `shutdownMarginSeconds` 1 to 300 are placeholders.

## Non-goals

- Rescheduling a session onto another agent when its agent disappears. Today such sessions
  go to `Failed` only if their `AgentService` is deleted; that behaviour is unchanged.
- Making sessions survive involuntary failures (OOM kill, node loss). Draining only covers
  voluntary disruption.
