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
