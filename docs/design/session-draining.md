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

## Principles

1. **Assume unmodified agents.** The operator must work, and never make things worse, for
   an agent that knows nothing about AgentOps. Anything that needs agent cooperation is
   opt-in and off by default.
2. **Do not rely on in-memory session state**, but serve agents that have it.
3. **Support both transports**: one long-lived connection (WebSocket, SSE, gRPC stream) or
   many independent requests.

State and transport are declared per `AgentService` rather than assumed.

## Three separate concerns

1. **Stop new work reaching a draining pod.**
2. **Let existing sessions finish**, up to a bounded time.
3. **Prefer killing idle pods** when the choice is ours (scale-in only).

Only the first two are required for correctness.

## Declaring the session model

```yaml
spec:
  sessions:
    state: Stateful          # default. Stateless = no in-memory session state
    transport: Connection    # Connection | Request
  lifecycle:
    drain:
      enabled: false         # opt-in: the agent implements the drain contract (step 3)
      timeoutSeconds: 300    # max time a draining agent may keep serving sessions
      shutdownMarginSeconds: 20
```

`state` and `transport` are independent questions:

- `state` is whether losing the pod loses data. `Stateful` is the default because it is the
  safe assumption: the operator never treats an agent as freely killable unless the user
  says it is.
- `transport` is whether losing the pod interrupts something in flight. A long-lived
  stream is cut by any pod removal whether or not the agent holds state, so connection
  draining applies to every `Connection` agent, `Stateless` included.

| `state` | `transport` | Baseline (unmodified agent) | With `drain.enabled` |
|---|---|---|---|
| `Stateless` | `Request` | best effort: endpoint removal is given time to propagate; in-flight requests finish only if the agent handles `SIGTERM` | in-flight requests finish within a short drain |
| `Stateless` | `Connection` | streams are cut on pod removal; clients reconnect elsewhere, nothing is lost | `/readyz` fails; streams get time to close |
| `Stateful` | `Connection` | streams **and their state** are cut | as above; state lives as long as the connection |
| `Stateful` | `Request` | not safe | not safe without session-aware routing (see Routing) |

The operator reports this instead of pretending: a `DrainSafe` condition on the
`AgentService`. It is `True` only when there is an explicit guarantee, which means
`drain.enabled` on a combination the drain covers (`Stateless` + `Request`, or any
`Connection`). Otherwise it is `False` with a reason:

| Reason | When |
|---|---|
| `BestEffortOnly` | `Stateless` + `Request` on an unmodified agent: nothing is lost, but an in-flight request can be |
| `DrainNotEnabled` | `Connection` on an unmodified agent |
| `RequestTransportWithoutRouting` | `Stateful` + `Request`, until session-aware routing exists |

`BestEffortOnly` is low impact and informational; it exists so the condition never claims
more than the baseline delivers.

Failing `/readyz` removes the pod from the Service endpoints, which only affects **new**
connections. Established connections keep flowing in the usual kube-proxy modes. That is
correct for `Connection`. It is wrong for `Request`: HTTP keep-alive and HTTP/2 reuse
connections, so one session's requests would split between the draining pod (reused
connections) and other pods, and some would land on a pod that does not hold the session's
state.

## Routing: use Gateway API

Kubernetes' Gateway API is the right surface; building a router is out of scope. Facts
checked while writing this note:

- **Session persistence** exists in Gateway API (cookie, with header as an extended
  feature) but is in the **Experimental** channel, not Standard, so implementation support
  varies and it can change.
- **Consistent hashing is not part of Gateway API.** It is implementation-specific, for
  example Envoy Gateway's `BackendTrafficPolicy` (hash on client IP or a header).

So the operator should depend on Gateway API objects (`Gateway`, `HTTPRoute`) and treat the
hashing policy as a per-implementation extension, not hard-code one implementation.

Consistent hashing of a session identifier (a header or the `AgentSession` name) is a good
trade-off for *placing* sessions: no lookup table, and most sessions keep their pod when
pods change. It is not enough for draining on its own. A pure hash does not know a pod is
draining; removing the pod from the ring remaps its sessions, which is exactly the loss we
are avoiding. A draining pod must keep receiving the sessions it owns while being excluded
for new ones, so the router needs explicit pins for those sessions, and Gateway API has no
standard way to express that today. Adding or removing pods also remaps roughly `1/N` of
sessions even without draining. Whether a given implementation can keep sessions on a
draining backend was **not verified** here and must be checked per implementation.

### Why `Stateful` + `Request` needs routing first

With a plain ClusterIP Service every request may land on a different pod, so in-memory
state is already broken in steady state, before any scale-in. `sessionAffinity: ClientIP`
only partly helps (clients behind one NAT share a pod) and it defeats retries. So routing
has to come before drain support for this combination.

### Agent-side rejection of new sessions (not a general answer)

A draining agent could keep `/readyz` = 200, keep serving sessions it knows, and answer
requests that start a new session with `503` + `Retry-After`. The Service does **not**
retry: kube-proxy balances per connection and ignores HTTP status codes. The client must
retry on a *new* connection; a keep-alive retry, or `sessionAffinity: ClientIP`, goes back
to the same pod. It also needs agent changes, so it is a fallback, not the plan.

## Proposal, in order

### Step 1: baseline for unmodified agents (no agent change)

- `strategy.rollingUpdate.maxUnavailable: 0`, so a rollout never lowers capacity below the
  desired count.
- A short `preStop` sleep so endpoint removal propagates before the container is stopped.
  The native `sleep` action needs no binary in the image: beta since Kubernetes 1.30, GA in
  1.34. Detecting cluster support is an open detail.
- `terminationGracePeriodSeconds` stays at Kubernetes' default. Raising it by default
  would make every rollout wait the full period for any agent that ignores `SIGTERM`, which
  is common for a process running as PID 1 without a handler.
- The `DrainSafe` condition above, and a README section stating what is and is not
  guaranteed for an unmodified agent.

### Step 2: scale on sessions, and scale in slowly (no agent change)

Wire `agentops_agentservice_active_sessions` into scaling (KEDA `prometheus` trigger or HPA
external metric). The gauge is computed by the operator, so this needs no agent change.
For `Stateful` agents use a long scale-down stabilization window by default; `Stateless`
agents may use the normal one. This reduces how often scale-in happens but does **not**
choose which pod dies, so it is not a safety mechanism by itself.

### Step 3: drain contract (opt-in, needs agent changes)

Enabled per `AgentService` with `lifecycle.drain.enabled: true`. The agent contract:

| | |
|---|---|
| `SIGTERM` | **must** start draining and exit `0` once idle or within `shutdownMarginSeconds`. A grace period alone does not make an agent finish work |
| `GET /drain?wait=<seconds>` on the `admin` port | starts draining (idempotent) and returns when the agent is idle or `wait` elapses. Used by the `preStop` hook |
| `GET /metrics` | optionally adds `agent_active_sessions` (gauge) for per-pod load |

While draining an agent rejects new sessions and, for `transport: Connection`, fails
`/readyz`.

The grace period is derived from what the user configures, so it can never be smaller than
the margin:

```
terminationGracePeriodSeconds = drain.timeoutSeconds + drain.shutdownMarginSeconds
preStop wait                  = drain.timeoutSeconds
```

| Setting | Default (when enabled) | Bounds |
|---|---|---|
| `drain.timeoutSeconds` | `300` for `Stateful` or `Connection`; `10` for `Stateless` + `Request` | 1 to 3600 (proposed cap) |
| `drain.shutdownMarginSeconds` | `20`, matching the echo agent's 20 s shutdown timeout | 1 to 300 (proposed) |

The minimum of 1 keeps the wait positive without a special case. An agent that needs
longer than the margin to exit must raise it.

**`/drain` must not be reachable from outside the pod network.** The generated Service
forwards the agent port and may be `NodePort` or `LoadBalancer`, so an unauthenticated
`/drain` on that port would let any caller drain agents. The operator instead adds a
separate container port named `admin` (default `9101`) that is **never** included in the
generated Service. Service ports are built from an explicit list, so this is a matter of
not adding it. The kubelet's `httpGet` hook reaches the pod IP directly. Residual risk:
other pods in the cluster can still reach the pod IP, so ship a `NetworkPolicy` that admits
only the node and namespace that need it, and require agents to serve `/drain` on the admin
port only.

The `preStop` hook **triggers** draining and then waits for idle. A Kubernetes `httpGet`
hook can only issue `GET`, but it does block termination until the handler returns, which
is why `/drain` is a blocking `GET` and needs no extra binary. The grace-period clock
starts *before* `preStop` runs and `SIGTERM` is sent only after it finishes, so the wait
must end before the grace period does; the derived values above guarantee the full margin
is left for `SIGTERM`. Update the README line about `/readyz` to say that failing readiness
is the drain signal for `transport: Connection` only.

### Step 4 (optional, best effort): pod-deletion-cost

If per-pod load is known, the operator can annotate pods with
`controller.kubernetes.io/pod-deletion-cost` so the ReplicaSet controller prefers idle
pods on scale-in. Caveats: it is only a preference, it costs an API write per pod per
change (rate-limit it), it needs `pods` get/list/patch RBAC, and it needs a per-pod load
source: agent-reported (opt-in) or possibly per-endpoint statistics from the gateway (not
verified).

## Open decisions

Settled: default `Stateful`, `Stateless` opts out; both transports are declared per
`AgentService`, and the `Stateless` and `Connection` combinations are supported;
`Stateful` + `Request` is conditional on draining-pin support (decision 1); the drain
budget is its own setting and draining is opt-in; routing goes through Gateway API with
consistent hashing as the placement strategy; idle is agent-reported when draining is on.

1. **Pins for draining pods.** Gateway API cannot express "keep serving these sessions on a
   draining backend". Which implementation do we target first, and can it do this?
2. **Reporting sessions cut by a drain timeout, without touching agents.** An unmodified
   agent reports nothing, and the operator cannot tell which pod held a session. It could
   mark such sessions `Failed` with a reason like `DrainTimeout` only if it owns the
   session-to-pod pins (and the router honors them), because then it knows which sessions
   a terminating pod held. Otherwise sessions stay `Bound` until `maxDurationSeconds` or
   the client completes them. Agent reporting stays an opt-in extra.
3. **Cluster support for the `preStop` sleep.** Detect support, or require Kubernetes 1.30+?
4. **Are the proposed caps right?** `drain.timeoutSeconds` max 3600 and
   `drain.shutdownMarginSeconds` 1 to 300 are placeholders.

## Non-goals

- Rescheduling a session onto another agent when its agent disappears. Today such sessions
  go to `Failed` only if their `AgentService` is deleted; that behaviour is unchanged.
- Making sessions survive involuntary failures (OOM kill, node loss). Draining only covers
  voluntary disruption.
