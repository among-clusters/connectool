# ConnecTool Helm chart

This chart defines one reviewable AI-tool trust chain:

`client plugin -> OIDC public PKCE client -> ToolHive vMCP -> reviewed MCP workloads`

It creates ToolHive `MCPGroup`, `MCPOIDCConfig`, and `VirtualMCPServer`
resources plus non-secret contracts for OIDC clients, ToolHive Registry sources,
and Codex marketplace/plugin metadata. Skills are distributed by a Git-backed
marketplace; ToolHive distributes MCP capabilities.

The chart does not install an identity provider, mutate its Secret, install
plugins on desktops, or store OAuth tokens. All endpoints, namespaces, registry
sources, publications, authorization policies, marketplace coordinates, and
optional ToolHive Registry settings are values-controlled.

Each publication may also declare `outgoingAuth` backend mappings and
`operational.failureHandling`. This keeps backend credentials referenced through
`MCPExternalAuthConfig` objects and makes health-check, best-effort, and circuit
breaker policy part of the Helm release instead of an imperative patch.

`toolhive.placement` is the single placement contract inherited by every
publication. It supports node selectors, affinity, tolerations, topology spread
constraints, and a resource budget for the `vmcp` container. Publications
default to one replica: colocated replicas are not node-level high availability.
Referenced MCP workload owners must apply the same placement boundary to their
own pod templates.

`toolhive.sessionStorage.provider` accepts `memory` or `redis`. Memory-backed
sessions require `ClientIP` affinity and trade session survival for removal of
a shared datastore dependency; clients reconnect after failover. Redis-backed
sessions retain the address and Secret-reference contract.

When `toolhive.runtimePolicy.enabled` is true, ConnecTool installs narrowly
scoped admission policies for ToolHive-generated child resources in the target
namespace. They spread children across the configured nodes, bound Deployment
rollouts, apply proxy resource budgets, and prefer same-node Service endpoints.
Backend Services use `sessionAffinity: None` so a proxy cannot remain pinned to
a remote backend; proxy and vMCP Services keep their CR-declared client affinity.
The policy is disabled by default because it creates cluster-scoped resources.

```sh
helm lint . -f examples/example-values.yaml
helm template connectool . -n connectool -f examples/example-values.yaml
```

## Public delivery

The canonical OCI artifact is:

```text
oci://ghcr.io/re8ch/charts/connectool
```

The GitHub release tag is `v<Chart.yaml version>`. GitHub Actions validates,
packages, and publishes the immutable SemVer OCI artifact. Deployments may use
any OCI mirror, but no private mirror is an upstream dependency.

RE8CH endpoints, identities, policies, and infrastructure values live only in
the downstream service-backend overlay.
