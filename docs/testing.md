# Testing

The provider has three test layers. Only the unit layer runs in `mise run ci`;
the e2e layer runs on demand and the live layer is strictly opt-in.

| Layer | Command | Needs | What it proves |
|---|---|---|---|
| Unit | `mise run test` | nothing | Provider logic against httptest fakes and the controller-runtime fake client |
| E2E (local) | `mise run e2e` | envtest binaries (auto-downloaded) | Real reconciler + real CloudProvider against a real kube-apiserver and a stateful Hetzner fake |
| E2E (live) | `E2E=1 HCLOUD_TOKEN=... mise run e2e` | a Hetzner Cloud project | The same round-trip against the real Hetzner Cloud API |

## Unit tests

```bash
mise run test
```

Race-enabled, self-contained, and always run by `mise run ci`.

## E2E suite (local)

```bash
mise run e2e
```

The suite lives in `test/e2e/` and is built from three pieces:

- **`test/e2e/fakehcloud/`** — a stateful, in-memory Hetzner Cloud API. Unlike
  the per-test handlers in the unit tests, create/get/list/delete share one
  catalog, so multi-step scenarios (create → get → drift → delete) observe
  consistent state. It mirrors the wire schema of the real API and enforces
  its label rules (format validation plus the reserved `hetzner.cloud`
  namespace), so label regressions fail here without a live account.
- **controller-runtime envtest** — a real `kube-apiserver` + `etcd` started
  from the binaries in `KUBEBUILDER_ASSETS`, with the CRDs from
  `charts/karpenter-provider-hetzner/crds` applied. The real
  `HCloudNodeClass` reconciler runs under a real manager (same registration
  shape as `cmd/controller/main.go`).
- **Scenario tests** — `TestE2E_NodeClassAndNodeLifecycle` (NodeClass
  reconcile → `Ready=True` with `status.resolvedImages`, then
  `CloudProvider.Create → Get → IsDrifted → Delete`, asserting labels,
  ProviderID, capacity/allocatable, and the `NodeClaimNotFound` contract) and
  `TestE2E_NodeClassNotReadyBlocksCreate` (a NodeClass the reconciler cannot
  make Ready blocks provisioning). `TestRealHetznerLifecycleConfiguration`
  and the smoke-helper tests run the live path's logic against local fakes so
  it stays covered without credentials.

### envtest binaries

`mise run e2e` downloads `kube-apiserver` and `etcd` through `setup-envtest`
on first run. Standalone:

```bash
mise run envtest-setup          # prints the binary path
```

Or point `KUBEBUILDER_ASSETS` at a directory you already have. Without the
binaries the envtest scenarios **skip cleanly** (they never fail), so
`mise run test` and `mise run ci` stay green on machines that never run the
e2e layer.

## Live smoke (opt-in)

```bash
E2E=1 HCLOUD_TOKEN=... mise run e2e
```

`TestE2E_RealHetzner_Lifecycle` runs the full CloudProvider round-trip
against `api.hetzner.cloud`: create one server, wait until it is running and
unlocked, `Get`, `IsDrifted`, delete, and confirm absence. The Kubernetes
side stays local (a controller-runtime fake client), so no cluster or
kubeconfig is needed.

| Variable | Default | Meaning |
|---|---|---|
| `E2E` | unset | Must be `1` to enable the live test |
| `HCLOUD_TOKEN` | unset | API token for the target project (read + write) |
| `E2E_SERVER_TYPE` | auto | Pin a server type; when unset the test picks the cheapest type Hetzner reports available at `E2E_LOCATION` |
| `E2E_LOCATION` | `fsn1` | Target location |
| `E2E_CLUSTER_NAME` | `karpenter-e2e` | Value for the `karpenter.sh/cluster` ownership label |

Notes and warnings:

- **It creates and deletes one real, billed server** (IPv6-only, so no IPv4
  surcharge; typically seconds old and a fraction of a cent). Clean-up is
  registered before any assertion, retries failed deletes, and confirms
  absence; a clean-up failure fails the test and prints the provider ID so
  the server can be removed by hand.
- The boot image is the newest snapshot matching the selected type's
  architecture **whose captured disk fits the type**, falling back to the
  `ubuntu-24.04` system image. Oversized snapshots are skipped because the
  live API refuses the boot ("image disk is bigger than server type disk").
- The Hetzner catalog churns: types vanish (`cx22`) and availability flags
  flip (`cx23`). Auto-selection follows live availability for this reason;
  a pinned `E2E_SERVER_TYPE` that is missing or out of stock fails fast with
  a descriptive error.

### Live-API constraint: reserved label namespace

The Hetzner Cloud API rejects any server label key containing
`hetzner.cloud` ("The hetzner.cloud/ prefix is reserved and cannot be
used"). The provider renames its two affected keys onto `karpenter.sh/*`
equivalents at the hcloud label boundary; NodePool requirement keys and
NodeClaim labels keep their canonical names. See
[`docs/adr/0002-hcloud-safe-label-keys.md`](adr/0002-hcloud-safe-label-keys.md).
The fake enforces the same rule so regressions fail in the local suite.

## Architecture decisions

- [`docs/adr/0001-e2e-testing-strategy.md`](adr/0001-e2e-testing-strategy.md) —
  why envtest + a stateful fake, and the skip-gating contract with CI.
- [`docs/adr/0002-hcloud-safe-label-keys.md`](adr/0002-hcloud-safe-label-keys.md) —
  the label-boundary rename.
