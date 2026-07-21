# Distribution

## Tagging

Releases follow [Semantic Versioning](https://semver.org/). Each tag is a
full version (e.g. `v0.1.0`, not `0.1`).

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

Pre-1.0 releases may break without notice. Pin a specific version in
production.
