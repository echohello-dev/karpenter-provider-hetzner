# Distribution

## Tagging

Releases follow [Calendar Versioning](https://calver.org/) in the
`YYYY.M.patch` shape: major is the year, minor is the month (unpadded —
SemVer numeric identifiers may not carry a leading zero), and patch
increments within the month.

[release-please](https://github.com/googleapis/release-please) cuts each
release from the conventional commits on `main` and pushes a
`v`-prefixed tag, so the current release is tagged `v2026.9.2` — `v`
plus the full bare version that `.release-please-manifest.json` and
`CHANGELOG.md` record (`2026.9.2`; a matching unprefixed alias tag is
sometimes added by hand). Conventional-commit bumps map onto the scheme:

- `fix:` → `YYYY.M.<patch+1>`, e.g. `v2026.9.1` → `v2026.9.2`
- `feat:` → `YYYY.<month+1>.0`, e.g. `v2026.9.2` → `v2026.10.0`
- `feat!:` → `<year+1>.0.0`; crossing into January needs a manual
  `Release-As:` trailer, since SemVer has no month or year rollover.

Historical `v0.1.x` tags predate the switch to CalVer. Container image
tags mirror the release tag (`karpenter-provider-hetzner:v2026.9.2`),
with a floating `v2026.9` and `latest` published alongside.

## Artifacts

A single goreleaser pipeline produces, per release:

- Multi-arch container image: `ghcr.io/echohello-dev/karpenter-provider-hetzner:vX.Y.Z`
  (and `-linux-amd64`, `-linux-arm64` variants).
- OCI Helm chart: `oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner:vX.Y.Z`.
- SBOM (`karpenter-provider-hetzner-sbom.spdx.json` per arch).
- Cosign signatures against the image digest.
- SLSA Level 3 provenance.

Multi-arch images and the helm chart are stored as
[GitHub Release](https://github.com/echohello-dev/karpenter-provider-hetzner/releases)
assets. The chart is also published to the OCI registry.

## Install

```bash
helm install karpenter-provider-hetzner \
  oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner \
  --version vX.Y.Z \
  --namespace karpenter \
  --set clusterName=$CLUSTER_NAME
```

CalVer carries no major-version compatibility signal, so any release may
break without notice. Pin a specific version (e.g. `--version
v2026.9.2`) in production.
