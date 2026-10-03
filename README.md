# karpenter-provider-hetzner

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud). It provisions, bin-packs, and autoscales Hetzner Cloud servers as Kubernetes nodes, picking the cost-optimal server type for the pending pods from Hetzner's real-time pricing.

## Status

**Alpha (pre-1.0).** The controller embeds Karpenter v1.14, reconciles HCloudNodeClass resources, resolves images and dependencies, prices Hetzner offerings, and manages server lifecycle and drift. The implementation is usable for testing, but production hardening and end-to-end cluster coverage are still in progress.

## Layout

```
cmd/controller/                # entrypoint binary
pkg/apis/v1/                   # HCloudNodeClass CRD types
pkg/cloudprovider/             # Karpenter CloudProvider implementation
pkg/providers/instance/        # hcloud server lifecycle
pkg/providers/instancetype/    # server types → priced Karpenter InstanceTypes
pkg/providers/pricing/         # per-type hourly cost
pkg/providers/imagefamily/     # Talos/Ubuntu snapshot resolution
pkg/controllers/nodeclass/     # HCloudNodeClass reconciler
pkg/metrics/                   # Prometheus counters
charts/karpenter-provider-hetzner/   # OCI-installable Helm chart
examples/                      # sample manifests
docs/                          # operator handbook (see docs/README.md)
```

## Quick start (skeleton)

```bash
# 1. Trust the mise manifest and install toolchains.
mise trust
mise install

# 2. Build, test, lint.
mise run ci

# 3. Run the controller against a cluster.
kubectl create namespace karpenter
kubectl -n karpenter create secret generic hcloud-token \
  --from-literal=token="$HCLOUD_TOKEN"
HCLOUD_TOKEN="$HCLOUD_TOKEN" \
  CLUSTER_NAME=my-cluster \
  ./bin/karpenter-provider-hetzner

# 4. Apply an HCloudNodeClass and NodePool.
kubectl apply -f examples/talos-nodeclass.yaml
```

## Testing

Unit tests run against an httptest-backed Hetzner Cloud fake and the controller-runtime fake client:

```bash
mise run test
```

The end-to-end suite (`test/e2e`) lifts that fake into a multi-step scenario harness and drives the real reconciler and CloudProvider against a real kube-apiserver (controller-runtime envtest) with the CRDs from `charts/karpenter-provider-hetzner/crds`:

```bash
mise run e2e
```

It covers the full local story — `HCloudNodeClass` reconcile → `Ready=True` with `status.resolvedImages` populated, then `CloudProvider.Create → Get → IsDrifted → Delete` asserting labels, ProviderID, and capacity/allocatable. No Hetzner account or Kubernetes cluster is required. The first run downloads the envtest binaries (`kube-apiserver`, `etcd`) through `setup-envtest`; `mise run envtest-setup` does that download standalone. Without those binaries (or `KUBEBUILDER_ASSETS`) the envtest scenarios skip cleanly, so `mise run test` and `mise run ci` stay green on machines that never run them. Smoke-test regression scenarios use a local HTTP fake and run without envtest binaries or Hetzner credentials.

An opt-in smoke test also runs the same round-trip against the **live Hetzner Cloud API**, creating and deleting one real billed server (IPv6-only, cheapest available type by default):

```bash
E2E=1 HCLOUD_TOKEN=... mise run e2e
```

Optional knobs: `E2E_SERVER_TYPE` (optional pin; when unset the test auto-selects the cheapest server type Hetzner reports available at `E2E_LOCATION`), `E2E_LOCATION` (default `fsn1`), `E2E_CLUSTER_NAME` (default `karpenter-e2e`). The boot image matches the selected server type's architecture and disk size, using the newest compatible snapshot or a compatible `ubuntu-24.04` system image. The test only runs when both `E2E=1` and `HCLOUD_TOKEN` are set. It waits for server creation to finish, and cleanup runs even when assertions fail, retrying deletion and confirming absence within its two-minute timeout. Cleanup failure fails the test and reports the server's provider ID.

See [`docs/testing.md`](docs/testing.md) for the full testing guide (layers, environment variables, live-API gotchas).

## Naming

| | |
|---|---|
| Repo | `echohello-dev/karpenter-provider-hetzner` |
| Go module | `github.com/echohello-dev/karpenter-provider-hetzner/v1` |
| CRD API group | `karpenter.hetzner.cloud` |
| NodeClass kind | `HCloudNodeClass` |
| Image | `ghcr.io/echohello-dev/karpenter-provider-hetzner` |
| Helm chart | `oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner` |

## License

Apache 2.0.

## Contributing

See `CONTRIBUTING.md`.
