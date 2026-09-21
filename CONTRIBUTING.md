# Contributing

Changes should preserve ConnecTool's provider-neutral, explicit configuration.

1. Do not add organization-specific endpoints, identities, credentials, or registry hosts.
2. Put deployment-specific settings in a downstream values file.
3. Run `helm lint charts/connectool --values charts/connectool/examples/example-values.yaml`.
4. Run `helm template connectool charts/connectool --values charts/connectool/examples/example-values.yaml`.
5. Update the chart version when publishing a release.

Maintainer-authored, same-repository pull requests receive the
`re8ch-policy-reviewer` check after verified commits and CI pass. The
App-owned check is the merge gate; forked or external-author pull requests
need a maintainer to bring the accepted change into a new trusted, signed PR.

Release tags use `v<chart-version>`. CI publishes the packaged chart to the public
GitHub Container Registry namespace and attaches the package to the GitHub release.
