# Native source and release correlation

The optional Sentry step in the existing product release workflow records
`connectool-catalog-gateway@<full source SHA>`. Runtime `SENTRY_RELEASE` must use
that exact value for the image's verified provenance source, even if runtime
configuration is delivered from a later GitOps commit. A tag or image digest is
an artifact reference, not a substitute for its source revision.

Configure the regular Sentry GitHub integration for the selected source
repository, then add that repository in the Sentry organization. The integration
supplies repository commits and pull-request associations. An SDK release field
alone does not demonstrate that a commit or PR was linked.

Repository Actions configuration:

- `SENTRY_AUTH_TOKEN`: approved release-automation token in GitHub Secrets; never
  a DSN, a developer's token in a file, workflow argument, or committed value.
- `SENTRY_ORG` and `SENTRY_PROJECT`: non-secret GitHub Variables for the existing
  organization/project.
- `SENTRY_RELEASE_ENABLED=true`: enable after the integration and token exist.

The pinned official Sentry release action sets commits using the existing full
Git checkout. It does not create another image, tag, controller or planner. It
runs only after the existing signed artifact release succeeds. It deliberately
omits `environment`: publishing an image is not evidence of production
installation. Record a Sentry deploy only after GitOps actual-image/Ready readback.

For an already published source, attach release metadata through the official
Release API with `refs: [{repository, commit}]` using the existing source SHA.
Do not rerun an image release merely to backfill this metadata.

Acceptance requires reading back the actual event's release and trace, the
Sentry release's commits, and the associated commit's native `pullRequest` field.
A configured step, a successful build or a nonempty SDK release string is not
proof of that association. Missing integration/repository access remains explicit.

References:
- https://docs.sentry.io/integrations/source-code-mgmt/github/
- https://docs.sentry.io/api/releases/create-a-new-release-for-an-organization/
- https://github.com/getsentry/action-release/tree/v3.9.1

## Existing integration and the remaining owner step

The verified organization inventory has the regular Sentry integration for
`re8ch` (Sentry integration `439047`) and registered `re8ch/service-backend`.
It does not have `among-clusters/connectool`. GitHub also reports no Sentry App
installation in `among-clusters`. Do not reuse the `re8ch` installation ID as if
it authorized this other organization.

An owner must open
https://sentry.io/settings/re8ch-studio/integrations/github/ and choose **Add
Installation**, select GitHub organization **among-clusters**, **Only select
repositories**, and **connectool** (repository ID `1356151877`) and **among-clusters**
(repository ID `1353608022`, for the independently instrumented Hub/Broker product).
Keep the installation limited to these two source repositories. The regular
native app is https://github.com/apps/sentry (not a new custom app). Return to
Sentry and register exactly `among-clusters/connectool` and
`among-clusters/among-clusters` in that installation.
There is no claimed preauthorized URL that can silently choose a repository or
replace the native owner approval. This remains incomplete until both GitHub
installation and Sentry repository API readback agree.

The official integration requests its native fixed permissions: administration
read; checks, commit statuses, contents, issues, pull requests and webhooks write;
metadata, organization members and account email read. Scope the installation to
the selected repository instead of adding an organization-wide custom writer.
These are the upstream App's requirements, not permissions added by this workflow.

Organization Tokens are recommended for ongoing release automation, but are not
an API requirement: the official Release API also accepts an approved bearer
user token with `project:releases` (or its documented alternative scopes). Do not
create extra permissions just because an Organization Token is not yet available.
Keep any authorized short-term token in the approved GitHub Secret and replace
it through normal rotation later. The runtime receives only its public DSN;
release tokens never belong in a Pod.
