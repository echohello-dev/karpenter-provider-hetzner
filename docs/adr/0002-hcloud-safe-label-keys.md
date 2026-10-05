---
number: 0002
date: 2026-10-03
raising_team: karpenter-provider-hetzner maintainers
prepared_by: johnnyhuy@users.noreply.github.com
status: accepted
---

# 0002. Hetzner-safe label keys at the hcloud boundary

## Context

The live Hetzner Cloud API rejects every server create whose labels contain
a key in the reserved `hetzner.cloud` namespace, failing with `invalid_input`
("The hetzner.cloud/ prefix is reserved and cannot be used"). Any key
containing the string is refused — prefixed (`karpenter.hetzner.cloud/...`)
and dotted (`karpenter.hetzner.cloud....`) forms alike.

Two provider-owned labels hit this reservation:

- the Karpenter NodeClass label, `karpv1.NodeClassLabelKey` for the CRD
  group, i.e. `karpenter.hetzner.cloud/hcloudnodeclass`;
- the server-family label, `karpenter.hetzner.cloud/server-family`, which
  `resolvedLabels` writes from the instance-type requirement key.

The result was that `CloudProvider.Create` could never succeed against real
Hetzner. Karpenter's canonical key shapes (derived from the CRD API group
`karpenter.hetzner.cloud`) are public surface — NodePool requirement
selectors in `examples/` and NodeClaim labels use them — so simply renaming
the keys everywhere would break user manifests and diverge from Karpenter's
standard NodeClass label convention on the Kubernetes side.

## Decision

Translate the affected keys **only at the Hetzner label boundary**:

- `toHcloudLabelKey` in `pkg/cloudprovider` maps the two canonical keys onto
  `karpenter.sh/hcloudnodeclass` and `karpenter.sh/server-family` when
  building the hcloud server label set (`buildServerLabels`).
- The round-trip read (`serverToNodeClaim`) fetches the server-side keys
  through the same mapping and reports the canonical keys on the NodeClaim
  again.
- Everything else keeps canonical names: NodePool requirement keys (see
  `examples/nodepool.yaml`), NodeClaim labels, and the scheduling
  requirement the instance-type provider publishes.

Guarding the decision:

- `test/e2e/fakehcloud` enforces the reservation (and hcloud label-format
  rules) so the local suite fails if a reserved key is ever emitted again.
- The flagship e2e scenario asserts no server label uses the namespace, and
  the unit tests assert the renamed keys appear where expected.

## Consequences

- Positive:
  - `CloudProvider.Create` works against the real Hetzner API (verified by
    the opt-in live smoke test).
  - No user-facing break: NodePool selectors and NodeClaim labels are
    unchanged; only the Hetzner-side label set differs.
  - The failure class is now caught offline by the fake's validation.
- Negative / trade-offs:
  - Servers created by the provider carry `karpenter.sh/*` keys rather than
    the CRD-group-derived keys, so anyone keying external tooling off
    `karpenter.hetzner.cloud/*` server labels must use the `karpenter.sh/*`
    forms (or the Kubernetes NodeClaim labels, which stay canonical).
  - Two key vocabularies exist (canonical vs Hetzner-side) and must be kept
    in sync via `toHcloudLabelKey`; adding new provider-owned labels in the
    reserved namespace requires extending the mapping.
- Follow-ups:
  - If upstream Karpenter standardises the Hetzner-side key names, revisit
    the mapping (this ADR would be superseded).

## References
- https://docs.hetzner.cloud/#overview-labels
- docs/testing.md
- pkg/cloudprovider/cloudprovider.go (`toHcloudLabelKey`)
