# Design note: exposing a session to the outside world

Status: **the edge side is implemented** in
[`ai-agent-edge-tunnel`](https://github.com/SteadyInfraTech/ai-agent-edge-tunnel)
(`spec.sessionRef`, ai-agent-edge-tunnel#6), and both API groups are renamed to `steadyinfra.com`
(agentops-operator#9, ai-agent-edge-tunnel#5). It has not been run against a real cluster with both
operators together. The shared client library (decision 4) is not built yet. This note describes how
`ai-agent-edge-tunnel` (a separate operator that creates short-lived Cloudflare tunnels) attaches to
an `AgentSession` without the two projects sharing a repository, a Go module or a release cycle.

## Goal

A client that holds an `AgentSession` can reach its agent from outside the cluster over a
Cloudflare hostname that exists exactly as long as the session does, and is protected by Cloudflare
Access unless someone explicitly opts out.

## Principles

1. **The Kubernetes API is the only interface.** Neither operator imports the other's Go types.
   The edge operator reads `AgentSession` as `unstructured`; AgentOps does not know the edge
   operator exists.
2. **The session owns the lifecycle.** The tunnel is derived from the session, never the other
   way round. AgentOps keeps working, unchanged, when the edge operator is not installed.
3. **Secure by default.** An edge object is never public by omission (already enforced by
   `ai-agent-edge-tunnel`).
4. **One direction of dependency**: edge → agents.

## The contract

Everything the edge operator needs from AgentOps is already in `AgentSession`:

| Field read by the edge operator | Meaning |
|---|---|
| `status.phase` | `Bound` means there is a target to expose. `Pending` waits. `Completed`, `Expired`, `Failed` end the tunnel. |
| `status.endpoint` | In-cluster URL of the bound agent, `http://<svc>.<ns>.svc:<port>`. This becomes the connector's ingress target. The parser currently also accepts an omitted port (defaults to 80) and an optional trailing `/`; these are current behaviour, not guaranteed contract. `<ns>` must equal the tunnel's own namespace. |
| `metadata.uid` | Identity of the session the tunnel was created for (see below). |

`spec.maxDurationSeconds` is deliberately **not** read. AgentOps already turns it into a phase change
(`Expired`), including when it is extended or is `0` (no limit), so the edge operator follows `status.phase`
and never computes a lifetime of its own. See "Lifetime".

These fields are the **contract** and should be treated as API: renaming or reshaping them is a
breaking change for the edge operator. If `status.endpoint` ever becomes something other than a
plain `http(s)://host:port` URL (for example a per-pod address once session-to-pod routing exists,
see [session-draining](session-draining.md)), the edge operator must be told, so it is called out
here and not left implicit.

### What the tunnel does and does not protect

The tunnel exposes the **agent's Service**, not an individual session. A session is only a
binding to an `AgentService`, and an `AgentPool` can bind many sessions to the same one. Cloudflare
Access decides **who can reach the service**; it does not decide **which sessions they may use**.

**This design does not provide session isolation.** A user admitted through one session's tunnel
can reach the same endpoint another session uses. If sessions must be isolated from each other, the
agent has to authorize each request against its session itself, for example by requiring a
session identifier and checking it; the container contract in the README does not currently require
that. Until it does, give agents that serve different users separate `AgentService` objects.

AgentOps needs no change to support this. Optional, later: an `AgentSession` condition or
annotation that records the public hostname, so a client can learn it from the object it already
holds (see open decisions).

## The edge side (in `ai-agent-edge-tunnel`)

```yaml
apiVersion: edge.steadyinfra.com/v1alpha1     # see "API group" below
kind: AgentEdgeTunnel
metadata:
  name: session-8f92
spec:
  sessionRef: { name: support-session-8f92 }     # an AgentSession in the same namespace
  cloudflare: { zoneID: "...", subdomain: agent-8f92 }
  security: { allowedEmails: [user@company.com] }
  # ttl is optional here: with sessionRef it is only a hard upper bound (see "Lifetime")
```

### Lifetime

With `sessionRef`, **the session's phase ends the tunnel, not a timer.** Consequences:

- Extending `maxDurationSeconds` on a bound session needs no action: AgentOps moves the session to
  `Expired` at the new deadline and the tunnel follows. The edge object's `ttl` cannot change while
  Active, which is why a lifetime copied from the session would go stale.
- `maxDurationSeconds: 0` (no limit) means the tunnel lives until the session reaches a terminal
  phase or is deleted. Nothing in the edge operator needs a finite remaining lifetime.
- `spec.ttl`, if set, starts after provisioning succeeds and limits the tunnel's active lifetime,
  even if the session is still `Bound`. Provisioning retries can leave Cloudflare resources
  allocated, and the connector serving traffic, before the TTL starts. Exposure can exceed
  `spec.ttl` if the expiry is not recorded. The TTL is optional and is not a strict exposure
  bound.

### Pinning the session identity

At the moment the tunnel first sees a `Bound` session it records `metadata.uid` in its status
(`status.sessionUID`) and verifies it on every reconcile. If the session disappears, or a session
of the same name has a different UID, the tunnel is terminated and never re-bound. Without this, a
session deleted and recreated under the same name before the edge operator noticed would inherit
the old tunnel, including its Access policy, pointing at the replacement session's endpoint.

`spec.sessionRef` and the existing `spec.service` are mutually exclusive: `service` keeps the
current "expose this Service" behaviour for anything that is not an `AgentSession`.

Reconciliation, in addition to what the operator does today:

1. **Pending**: the session does not exist yet, is `Pending` (not yet `Bound`), or exposes an
   endpoint the edge operator will not accept. These are waited for only until the session's UID has
   been recorded in status; after that, see Terminated and the unexpected-phase rule below (a pinned
   session whose endpoint is rejected or changes terminates the tunnel). A session that is already
   `Completed`, `Expired` or `Failed` is not waited for: terminal beats pending, so an object first
   reconciled after the session ended goes straight to Terminated. Clients should treat only
   `Terminated` as final. While waiting for an unpinned session the operator provisions no
   Cloudflare resources; once pinned, Cloudflare resources can exist while the tunnel is still
   `Pending`. Status says what it is waiting for. A missing session is retried, not failed, so the two objects can be applied in
   either order.
2. **Active**: the session is `Bound`. Its UID and endpoint are recorded in status
   (`status.sessionUID`, `status.sessionEndpoint`) **before** anything is created, and the
   connector's ingress target is the recorded endpoint.
3. **Terminated**: the session reaches `Completed`, `Expired` or `Failed`, is deleted, has a
   different UID, is being deleted, or is bound to a different endpoint. The edge operator removes
   the connector, DNS, Access application and tunnel in its usual order, then marks the object
   `Terminated`. A session that merely reports an unexpected phase ends nothing: the tunnel keeps
   running and the TTL cap and connector upkeep continue.
4. **Re-binding ends the tunnel; it does not re-point it.** If the session's `status.endpoint`
   changes after the tunnel was bound, the tunnel is terminated. An earlier draft of this note
   updated the connector in place. cloudflared does not reload its config, and silently re-pointing
   an Access-protected hostname at a different agent is the riskier default. A client that needs a
   new hostname for the new binding creates a new `AgentEdgeTunnel`.

The session is read as `unstructured` and polled every 10 seconds, not watched, so the edge
operator starts without AgentOps installed. A tunnel can outlive its session by roughly that long, and longer if the controller, the API
server or Cloudflare cleanup is failing: the interval is a polling period, not a deadline. The
optional `spec.ttl` is also enforced by reconciliation, so it is a cap that holds while the operator
is healthy, not an independent cutoff.

Because the edge object only *reads* the session, it never needs write access to AgentOps
resources. RBAC: `get` on `agentsessions` in the namespaces it manages (polling needs no `list` or `watch`).

## API group

An API group should be a DNS name the project owns. `agentops.io` is not one, so both projects
move to `steadyinfra.com`, one group per concern:

| Project | Group |
|---|---|
| agentops-operator | `agents.steadyinfra.com` (was `agents.agentops.io`) |
| ai-agent-edge-tunnel | `edge.steadyinfra.com` (was the bare `agentops.io`) |

Both are `v1alpha1` and unreleased, so there is no migration tool. The renames are two separate,
**pending** PRs, one per repository ([agentops-operator#9](https://github.com/SteadyInfraTech/agentops-operator/pull/9),
[ai-agent-edge-tunnel#5](https://github.com/SteadyInfraTech/ai-agent-edge-tunnel/pull/5)). The label
and finalizer keys that used `agentops.io/` move with them.

**There is no supported migration, and objects must not be restored from a backup.** Restoring would
carry over server-generated metadata, status, owner references to old UIDs and the old finalizer
(`agentops.io/edge-tunnel-cleanup`), none of which the new operators understand. The new edge operator
does not recognise the old finalizer, so tunnels from the old group would be left with nothing
managing their Cloudflare resources. For a dev cluster that already has the old group:

1. Delete every `AgentEdgeTunnel` (old group) and **wait** until they are gone, so the old operator's
   finalizer removes the Cloudflare tunnel, DNS record and Access application. Fix anything stuck first.
2. Delete the old `AgentSession`, `AgentService`, `AgentPool` and `AgentRuntime` objects, then the old
   CRDs, then uninstall the old operators.
3. Install the new operators and write the objects again under the new groups, `AgentRuntime` first,
   renaming the pool label key in the service labels and each `AgentPool` `spec.selector` together.
4. Check the Cloudflare dashboard for tunnels, DNS records and Access applications left behind.

## Why not the alternatives

- **Add a `public:` block to `AgentService`/`AgentSession`** and have AgentOps call Cloudflare.
  Couples every user of AgentOps to Cloudflare credentials and a Cloudflare-specific API, and
  forces the two release cycles together.
- **Import AgentOps's Go types in the edge operator.** A shared module means shared versioning;
  `unstructured` plus four documented fields is a smaller, explicit contract.
- **A monorepo.** The projects have different blast radii (the edge operator holds a Cloudflare
  API token; AgentOps holds none) and should stay separately deployable and reviewable.

## Decisions

1. **Group names**: `agents.steadyinfra.com` and `edge.steadyinfra.com`. Decided.
2. **How the client learns the hostname**: it reads `status.hostname` of the `AgentEdgeTunnel`
   it asked for. `status.hostname` is set while the tunnel is still `Pending`, so the hostname alone
   does **not** mean the endpoint works: the client, and the shared library, must wait for
   `status.phase: Active` **and** `status.connectorReady: true`. Only `Terminated` is final.
   `Failed` is **not**: the edge controller sets it for transient provisioning errors and keeps
   retrying, and it also uses it for an invalid spec, which is not retried. The client keeps
   watching through `Failed`, surfaces `status.message`, and applies its own timeout rather than
   giving up on the first `Failed`. The edge operator does not write to `AgentSession`, so it needs no `patch` on
   sessions. If a client later needs the hostname on the session itself, an annotation can be
   added without changing the contract. Decided.
3. **Who creates the `AgentEdgeTunnel`**: the component that creates the `AgentSession`
   (a gateway, backend or platform layer), not an operator. It is the only party that knows the
   user's identity, and `security.allowedEmails` is exactly that identity; an automatic
   session-to-tunnel controller would have to guess it or leave the tunnel public. The creator
   sets an `ownerReference` from the `AgentEdgeTunnel` to the `AgentSession`, so Kubernetes
   deletes the tunnel when the session is deleted. The edge operator also tears the tunnel down
   when the session ends, so there are two independent safeguards. Decided.
4. **Shared code**: a small client library (not an operator) that creates a session and its
   edge object together, sets the owner reference, and returns the hostname, so every caller
   does not repeat it. It depends on both API types and on neither operator, and lives in its
   own module so the two operators stay independent. Not built yet.

## Still open

1. **Session-to-pod routing.** Today a session binds to a Service, so the tunnel exposes the whole
   service. Once `session-draining` introduces per-pod routing, `status.endpoint` must keep
   meaning "the address a client should use", and the edge operator should not need to change.
2. **Per-session Access identity.** `security.allowedEmails` is static. Deriving the allowed
   identity from `AgentSession.spec.userID` would tie the two, so it is deliberately left out.
