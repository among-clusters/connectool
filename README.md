# ConnecTool

ConnecTool is a provider-neutral control contract for distributing AI tools
through OIDC, ToolHive MCP workloads, and Codex-compatible plugin marketplaces.
It turns the full chain into reviewable, values-controlled Kubernetes resources
without embedding one organization's endpoints, policies, identities, or registry.

## Install

```sh
helm install connectool \
  oci://ghcr.io/re8ch/charts/connectool \
  --version 0.2.2 \
  --namespace connectool \
  --create-namespace \
  --values my-values.yaml
```

Start with [`charts/connectool/examples/example-values.yaml`](charts/connectool/examples/example-values.yaml).

## What it standardizes

- public PKCE client contracts for an OIDC provider;
- ToolHive Registry sources and optional registry-server release settings;
- MCP groups, OIDC configuration, virtual MCP aggregation, and authorization;
- Codex marketplace metadata, MCP ownership, and reusable skill-only plugins;
- validation that each publication has exactly one MCP-owning plugin.

ConnecTool stores no OAuth tokens or identity-provider secrets and installs no
desktop plugins. See [the architecture boundary](docs/architecture.md).

For memory-backed vMCP replicas behind load-balancing gateways, ConnecTool can
also deploy an optional stateless `Mcp-Session-Id` router. It preserves session
ownership across router replicas without Redis and prefers same-node upstreams.
