# Design note: exposing a session to the outside world

Status: **proposal; decisions recorded below.** Nothing here is implemented except the group rename (separate PRs). It describes how
[`ai-agent-edge-tunnel`](https://github.com/SteadyInfraTech/ai-agent-edge-tunnel) (a separate
operator that creates short-lived Cloudflare tunnels) attaches to an `AgentSession` without the
two projects sharing a repository, a Go module or a release cycle.

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
| `status.endpoint` | In-cluster URL of the bound agent, `http://<svc>.<ns>.svc:<port>`. This becomes the connector's ingress target. |
| `status.assignedAgent` | The `AgentService` name; shown in the edge object's status for debugging. |
| `spec.maxDurationSeconds`, `status.boundAt` | Default tunnel lifetime when the edge object sets no `ttl`. |

These fields are the **contract** and should be treated as API: renaming or reshaping them is a
breaking change for the edge operator. If `status.endpoint` ever becomes something other than a
plain `http(s)://host:port` URL (for example a per-pod address once session-to-pod routing exists,
see [session-draining](session-draining.md)), the edge operator must be told, so it is called out
here and not left implicit.

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
  # ttl is optional here: it defaults to the session's remaining lifetime
```

`spec.sessionRef` and the existing `spec.service` are mutually exclusive: `service` keeps the
current "expose this Service" behaviour for anything that is not an `AgentSession`.

Reconciliation, in addition to what the operator does today:

1. **Pending**: the session is not `Bound` (or does not exist yet). Nothing is created in
   Cloudflare; status says what it is waiting for. A missing session is retried, not failed, so
   the two objects can be applied in either order.
2. **Active**: the session is `Bound`. The connector's ingress target is taken from
   `status.endpoint`. The session reference and the resolved endpoint are recorded in status.
3. **Terminated**: the session reaches `Completed`, `Expired` or `Failed`, or is deleted. The
   edge operator removes the connector, DNS, Access application and tunnel in its usual order,
   then marks the object `Terminated`.
4. **Re-binding**: if the session's `status.assignedAgent` changes, the connector is updated to
   the new endpoint; the hostname and Access policy are unchanged.

Because the edge object only *reads* the session, it never needs write access to AgentOps
resources. RBAC: `get`, `list`, `watch` on `agentsessions` in the namespaces it manages.

## API group

An API group should be a DNS name the project owns. `agentops.io` is not one, so both projects
move to `steadyinfra.com`, one group per concern:

| Project | Group |
|---|---|
| agentops-operator | `agents.steadyinfra.com` (was `agents.agentops.io`) |
| ai-agent-edge-tunnel | `edge.steadyinfra.com` (was the bare `agentops.io`) |

Both are `v1alpha1` and unreleased, so this is a rename with no migration. It is done in two
separate PRs, one per repository (agentops-operator#9 and ai-agent-edge-tunnel#5). The label and
finalizer keys that used `agentops.io/` move with them.

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
   it asked for. The edge operator does not write to `AgentSession`, so it needs no `patch` on
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
