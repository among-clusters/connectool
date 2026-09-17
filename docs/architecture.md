# Architecture boundary

ConnecTool models one portable chain:

```text
Codex-compatible plugin
  -> OIDC public PKCE client
  -> ToolHive VirtualMCPServer
  -> reviewed MCP workloads and tool policies
```

The upstream chart owns only schemas, validation, non-secret contracts, and
ToolHive custom resources. Every environmental choice is a value: issuer,
namespaces, Registry sources, session storage references, public MCP URLs,
workloads, policies, marketplace source, and plugin-to-publication ownership.

It does not own identity-provider installation, Secret mutation, OAuth tokens,
desktop installation, network publication, or the MCP workload implementations.

## Placement and availability

Every publication inherits the single `toolhive.placement` contract. Placement
is rendered into the `VirtualMCPServer` pod template so an operator reconcile
cannot discard it. Referenced MCP workloads remain owned by their workload
manifests and must use the same application placement boundary.

The contract includes node selection, affinity, tolerations, topology spread,
and the `vmcp` container resource budget. A deployment should not tolerate node
network-agent readiness taints merely to force placement; node health is a
promotion gate, not a scheduling exception.

ConnecTool defaults publications to one replica. Multiple replicas on one node
provide only process redundancy and must never be described as high
availability. Do not combine single-node placement with required pod
anti-affinity. Node-level HA requires independent, health-gated origins before
replicas are distributed across failure domains.

Memory-backed sessions pair `ClientIP` affinity with reconnect-on-failover and
remove the shared session database from the availability path. Redis remains an
explicit option when session continuity is more important than node locality.
The optional runtime policy extends placement to ToolHive-generated child
workloads and uses same-node Service preference with remote-node fallback; it is
namespace- and label-scoped and is never enabled implicitly.

## Distribution

The canonical chart is public at:

```text
oci://ghcr.io/re8ch/charts/connectool
```

An adopter may mirror the artifact to any OCI registry. A mirror is an optional
transport/cache and never an upstream dependency. Environment overlays stay in
the adopter's own GitOps repository.

If ConnecTool later ships controller images, values must separate `registry`,
`repository`, `tag`, and `digest`; defaults must remain publicly pullable and
production consumers must be able to pin digests.
