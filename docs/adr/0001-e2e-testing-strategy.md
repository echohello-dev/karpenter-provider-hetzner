---
number: 0001
date: 2026-10-03
raising_team: karpenter-provider-hetzner maintainers
prepared_by: johnnyhuy@users.noreply.github.com
status: accepted
---

# 0001. End-to-end testing strategy: envtest plus a stateful Hetzner fake

## Context

The provider had unit tests only (httptest-backed hcloud fakes plus the
controller-runtime fake client), and the README admitted end-to-end cluster
coverage was still in progress. A genuine e2e layer had to bridge several
constraints:

- No real Hetzner account and no real cluster may be required for the
  default path; `mise run ci` must be unaffected on machines that never run
  the e2e suite.
- The code under test is controller-shaped: an `HCloudNodeClass` reconciler
  driven through a real API server (CRD schema, status subresources, merge
  patches) and a Karpenter `CloudProvider` that round-trips against the
  Hetzner Cloud API.
- The existing unit-test fakes are per-test and stateless, which blocks
  multi-step scenarios (create → get → drift → delete) from observing
  consistent state between calls.
- Hetzner-specific behaviours (label reservations, disk-fit rules, catalog
  availability) only surface against the real API or a faithful fake.

## Decision

Build the e2e layer in `test/e2e/` from three pieces:

1. **controller-runtime envtest** runs a real `kube-apiserver` and `etcd`
   with the CRDs from `charts/karpenter-provider-hetzner/crds`. The real
   reconciler is registered under a real manager, mirroring the wiring in
   `cmd/controller/main.go`, so the production controller path is exercised
   rather than a test double.
2. **A stateful fake Hetzner API** (`test/e2e/fakehcloud/`) lifts the
   unit-test httptest handlers into one in-memory catalog shared by create,
   get, list and delete. It encodes responses with the hcloud schema types,
   honours the real API's query filtering, and enforces its server-label
   rules (format validation and the reserved `hetzner.cloud` namespace) so
   live-API refusals are reproducible locally.
3. **An opt-in live smoke test** gated on `E2E=1` and `HCLOUD_TOKEN` runs the
   same CloudProvider round-trip against `api.hetzner.cloud`, with a local
   fake Kubernetes client and strict clean-up (retrying deletes and
   confirming absence). It auto-selects the cheapest available server type
   and a boot image that fits the type's disk, because the catalog churns.

Supporting decisions:

- **Skip, never fail.** Without `KUBEBUILDER_ASSETS` (or `E2E=1`) the
  relevant tests skip with actionable messages, so `go test ./...` and
  `mise run ci` stay green anywhere.
- **Binary provisioning through mise.** `mise run e2e` downloads envtest
  binaries via `setup-envtest` on first run; `mise run envtest-setup` does
  the same standalone. Nothing assumes the binaries are present.
- **Scenario coverage.** One flagship flow (NodeClass reconcile →
  `Ready=True` with `status.resolvedImages` → `Create → Get → IsDrifted →
  Delete` with label, ProviderID and capacity/allocatable assertions) plus a
  negative flow (a non-Ready NodeClass blocks provisioning), plus local
  regression tests for the live path's helpers.

## Consequences

- Positive:
  - Real CRD validation, real status subresource semantics and real
    controller wiring are exercised without a cluster or an account.
  - Multi-step scenarios observe consistent state, and drift injection
    (`SetServerImage`) is a first-class seam.
  - The fake's label validation turns live-only failures into local,
    deterministic ones (it caught the reserved `hetzner.cloud` namespace
    regression class before any live run needed to).
  - `mise run ci` cost is unchanged: the e2e layer compiles and skips.
- Negative / trade-offs:
  - envtest is not a full cluster (no kubelet, no CCM, no networking), so
    node registration and the Karpenter core controllers remain uncovered.
  - The fake tracks a snapshot of the hcloud API surface the provider uses;
    new endpoints must be added to it or they will 404 in scenarios.
  - The live smoke costs a fraction of a cent per run and needs a write
    token; it is therefore never part of the default run.
- Follow-ups:
  - Extend scenarios to user-data Secret resolution and placement-group
    spreading through the manager-driven path.
  - Consider a periodic scheduled live run to catch Hetzner catalog and API
    changes (types vanishing, availability flags flipping, new label rules).

## References
- https://book.kubebuilder.io/reference/envtest
- https://github.com/hetznercloud/hcloud-go
- docs/testing.md
