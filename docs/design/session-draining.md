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

## Three separate concerns

1. **Stop new work reaching a draining pod.**
2. **Let existing sessions finish**, up to a bounded time.
3. **Prefer killing idle pods** when the choice is ours (scale-in only).

They need different mechanisms, and only the first two are required for correctness.

## The crux: readiness and existing sessions

Failing `/readyz` removes the pod from the Service endpoints. That stops *new* connections
to the pod, but it also stops *existing sessions* from reaching it through the same Service
whenever a client opens a new connection (every plain HTTP request, and any reconnect).

So "drain by failing readiness" is only correct if each session is one long-lived
connection (WebSocket, SSE, gRPC stream) or if sessions are reached by some path that does
not depend on Service endpoints (headless Service / per-pod address / gateway). Otherwise
it breaks the sessions it is trying to protect. This is the routing question again, and the
README already lists sticky routing as future work.

Two ways out:

- **A. Agent rejects new sessions itself.** A draining agent keeps `/readyz` = 200, keeps
  serving requests for sessions it already knows, and answers requests that would start a
  new session with `503` + `Retry-After`. The Service load balancer then retries elsewhere.
  Works with plain ClusterIP routing. Needs the agent to distinguish "known session" from
  "new session", which requires a session identifier in requests.
- **B. Fail readiness and route around it.** Correct only with per-pod or gateway routing
  (see above). Simpler contract, bigger prerequisite.

## Proposal, in order

### Step 1: bounded termination (operator only, no agent change)

Add to `AgentServiceSpec` / `AgentRuntimeSpec`:

```yaml
lifecycle:
  terminationGracePeriodSeconds: 300   # default: Kubernetes' 30 s, unchanged
```

and set `terminationGracePeriodSeconds` on the pod template. Also set
`strategy.rollingUpdate.maxUnavailable: 0` so a rollout never lowers capacity below the
desired count. This makes the existing `SIGTERM` handling in agents meaningful (the echo
agent already shuts down gracefully with a 20 s timeout) and is safe by default because
the default value does not change.

### Step 2: scale on sessions, and scale in slowly

Wire `agentops_agentservice_active_sessions` into scaling (KEDA `prometheus` trigger or HPA
external metric) with a long scale-down stabilization window. This reduces how often
scale-in happens but does **not** choose which pod dies, so it is not a safety mechanism by
itself.

### Step 3: drain contract (needs the decisions below)

Extend the container contract:

| | |
|---|---|
| `SIGTERM` / `preStop` | agent enters draining: rejects new sessions (option A), keeps serving known ones, exits when idle or when the grace period ends |
| `GET /metrics` | adds `agent_active_sessions` (gauge) so the operator and KEDA can see per-pod load |

The operator adds a `preStop` hook that waits for the agent to report idle, bounded by the
grace period from step 1. Update the README line about `/readyz` to match whichever of
A/B is chosen.

### Step 4 (optional, best effort): pod-deletion-cost

If agents report per-pod load, the operator can annotate pods with
`controller.kubernetes.io/pod-deletion-cost` so the ReplicaSet controller prefers idle
pods on scale-in. Caveats: it is only a preference, it costs an API write per pod per
change (rate-limit it), it needs `pods` get/list/patch RBAC, and it is useless without the
per-pod load from step 3.

## Open decisions

1. **Do agents keep session state in memory?** If not (state in Redis/Postgres), none of
   this is needed beyond step 1; any replica can serve any session.
2. **What is the session transport?** One long-lived connection, or many independent
   requests? This decides between A and B above.
3. **How long may a drain take?** Compare with `maxDurationSeconds`. A one-hour grace
   period blocks node drains and rollouts; sessions that outlive the grace period will be
   cut regardless.
4. **Who decides a pod is idle?** Agent-reported (precise, needs the contract) or inferred
   by the operator (impossible today, since sessions are not bound to pods).

## Non-goals

- Rescheduling a session onto another agent when its agent disappears. Today such sessions
  go to `Failed`; that behaviour is unchanged.
- Making sessions survive involuntary failures (OOM kill, node loss). Draining only covers
  voluntary disruption.
