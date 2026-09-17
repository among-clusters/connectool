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
