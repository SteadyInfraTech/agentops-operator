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

`AgentSession.spec.maxDurationSeconds` defaults to 3600, so "wait for sessions to finish"
can mean an hour. Any design has to say how long it is willing to wait.

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
    terminationGracePeriodSeconds: 300   # example override; unset = Kubernetes' 30 s
```

`Stateful` is the default because it is the safe assumption: the operator never treats an
agent as freely killable unless the user says it is. `Stateless` opts out of drain
protections and allows normal scale-in.

| `state` | `transport` | Drain-safe? | How |
|---|---|---|---|
| `Stateless` | any | yes | nothing session-specific. Step 1 only lets in-flight requests finish |
| `Stateful` | `Connection` | yes | fail `/readyz` when draining starts; established connections keep flowing through kube-proxy, new ones go to other pods; wait for idle |
| `Stateful` | `Request` | **not with plain Service routing** | needs session-aware routing (see below) |

### Why `Stateful` + `Request` needs routing first

With a ClusterIP Service every request may land on a different pod, so in-memory state is
already broken in steady state, before any scale-in. `sessionAffinity: ClientIP` only
partly helps (clients behind one NAT share a pod) and it defeats retries: a retry from the
same client IP returns to the same pod. Draining such sessions safely needs the router to
know which pod owns a session (header or consistent-hash gateway, or per-pod addressing)
and to stop assigning new sessions to a draining pod. That is the sticky-routing work the
README already lists as future work, and it should come before drain support for this
combination. Until then the operator should report it rather than pretend:
a `DrainSafe=False` condition with reason `RequestTransportWithoutRouting`.

### Agent-side rejection of new sessions (not a general answer)

A draining agent could keep `/readyz` = 200, keep serving sessions it knows, and answer
requests that start a new session with `503` + `Retry-After`. Note the Service does **not**
retry: kube-proxy balances per connection and ignores HTTP status codes. The client must
retry on a *new* connection; a keep-alive retry, or `sessionAffinity: ClientIP`, goes back
to the same pod. This only helps clients that already retry correctly and does not fix
steady-state routing, so it is a fallback, not the plan.

## Proposal, in order

### Step 1: bounded termination (operator only, no agent change)

Add `lifecycle.terminationGracePeriodSeconds` to `AgentServiceSpec` / `AgentRuntimeSpec`
and set it on the pod template. Also set `strategy.rollingUpdate.maxUnavailable: 0` so a
rollout never lowers capacity below the desired count. This makes the existing `SIGTERM`
handling in agents meaningful (the echo agent already shuts down gracefully with a 20 s
timeout). It is safe by default because an unset value keeps Kubernetes' 30 s.

### Step 2: scale on sessions, and scale in slowly

Wire `agentops_agentservice_active_sessions` into scaling (KEDA `prometheus` trigger or HPA
external metric). For `Stateful` agents use a long scale-down stabilization window by
default; `Stateless` agents may use the normal one. This reduces how often scale-in
happens but does **not** choose which pod dies, so it is not a safety mechanism by itself.

### Step 3: drain contract (`Stateful` agents)

Extend the container contract:

| | |
|---|---|
| `GET /drain?wait=<seconds>` | starts draining (idempotent) and returns when the agent is idle or `wait` elapses. Used by the `preStop` hook |
| `SIGTERM` | also starts draining. Covers agents without `/drain` and a hook that failed |
| `GET /metrics` | adds `agent_active_sessions` (gauge) so the operator and KEDA can see per-pod load |

While draining an agent rejects new sessions and, for `transport: Connection`, fails
`/readyz`, then exits `0` once idle or when it is told to stop.

The operator adds a `preStop` hook that **triggers** draining and then waits for idle. A
Kubernetes `httpGet` hook can only issue `GET`, but it does block termination until the
handler returns, which is why `/drain` is a blocking `GET` and needs no extra binary in the
image. The grace-period clock starts *before* `preStop` runs and `SIGTERM` is sent only
after it finishes, so the wait must time out before the grace period ends. The operator
sets `wait = terminationGracePeriodSeconds - margin`; otherwise the kubelet would send
`SIGKILL` with no useful `SIGTERM` window. Update the README line about `/readyz` to say
that failing readiness is the drain signal for `transport: Connection` only.

### Step 4 (optional, best effort): pod-deletion-cost

If agents report per-pod load, the operator can annotate pods with
`controller.kubernetes.io/pod-deletion-cost` so the ReplicaSet controller prefers idle
pods on scale-in. Caveats: it is only a preference, it costs an API write per pod per
change (rate-limit it), it needs `pods` get/list/patch RBAC, and it is useless without the
per-pod load from step 3.

## Open decisions

Settled: agents may or may not keep state (default `Stateful`, `Stateless` opts out), and
both transports are supported (declared per `AgentService`).

1. **How long may a drain take?** Compare with `maxDurationSeconds` (default 3600). A
   one-hour grace period blocks node drains and rollouts, and sessions that outlive it are
   cut regardless. Does a session that outlives the grace period fail, or can the operator
   ask the agent to checkpoint?
2. **Routing for `Stateful` + `Request`.** Gateway with header or consistent-hash routing,
   or per-pod addressing? This is a prerequisite for that combination (see above).
3. **Who decides a pod is idle?** This note proposes agent-reported (steps 3 and 4). The
   alternative, operator-inferred, is impossible while sessions are bound to services
   rather than pods.

## Non-goals

- Rescheduling a session onto another agent when its agent disappears. Today such sessions
  go to `Failed`; that behaviour is unchanged.
- Making sessions survive involuntary failures (OOM kill, node loss). Draining only covers
  voluntary disruption.
