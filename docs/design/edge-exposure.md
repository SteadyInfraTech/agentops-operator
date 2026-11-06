# Design note: exposing a session to the outside world

Status: **proposal.** Nothing here is implemented. It describes how
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
apiVersion: edge.agentops.io/v1alpha1     # see "API group" below
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

Today the two projects use different groups for unrelated reasons:

| Project | Group |
|---|---|
| agentops-operator | `agents.agentops.io` |
| ai-agent-edge-tunnel | `agentops.io` (the bare parent domain) |

The bare `agentops.io` group is a poor home for anything: it claims the whole namespace, and it
looks like the parent of `agents.agentops.io`. Proposal: move `AgentEdgeTunnel` to
**`edge.agentops.io`**, one group per concern (`agents.`, `edge.`). Both projects are `v1alpha1`
and unreleased, so this is a rename with no migration. It is the only part of this note that needs
a coordinated change, and it only touches the edge repository.

## Why not the alternatives

- **Add a `public:` block to `AgentService`/`AgentSession`** and have AgentOps call Cloudflare.
  Couples every user of AgentOps to Cloudflare credentials and a Cloudflare-specific API, and
  forces the two release cycles together.
- **Import AgentOps's Go types in the edge operator.** A shared module means shared versioning;
  `unstructured` plus four documented fields is a smaller, explicit contract.
- **A monorepo.** The projects have different blast radii (the edge operator holds a Cloudflare
  API token; AgentOps holds none) and should stay separately deployable and reviewable.

## Open decisions

1. **Group name**: `edge.agentops.io` (proposed) or another name.
2. **Who tells the client the hostname.** Either the client reads the `AgentEdgeTunnel` it asked
   for, or the edge operator writes the hostname back onto the `AgentSession` (an annotation
   such as `edge.agentops.io/hostname`). Writing back means the edge operator needs `patch` on
   sessions; reading means a client needs access to two kinds. Proposal: start with reading; add
   the annotation only if a client needs it.
3. **Who creates the `AgentEdgeTunnel`.** A client or platform controller that creates sessions
   would create the matching edge object in the same transaction of intent. A small
   `AgentSession`-to-`AgentEdgeTunnel` controller is possible later but would reintroduce the
   coupling this note avoids, so it belongs in a third, optional component.
4. **Session-to-pod routing.** Today a session binds to a Service, so the tunnel exposes the whole
   service. Once `session-draining` introduces per-pod routing, the contract field
   `status.endpoint` must keep meaning "the address a client should use", and the edge operator
   should not need to change.
5. **Per-session Access identity.** `security.allowedEmails` is static. Deriving the allowed
   identity from `AgentSession.spec.userID` would tie the two, so it is deliberately left out of
   this note.
