# karpenter-provider-hetzner

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud). It provisions, bin-packs, and autoscales Hetzner Cloud servers as Kubernetes nodes, picking the cost-optimal server type for the pending pods from Hetzner's real-time pricing.

## Status

**Alpha (pre-1.0).** The interface contract against `sigs.k8s.io/karpenter` is wired and the CRD types reconcile end-to-end, but the providers in `pkg/providers/...` are deliberate stubs returning `not yet implemented`. Fill-in work is documented inline with `TODO:` markers; see `pkg/cloudprovider/cloudprovider.go` and `pkg/providers/{instance,instancetype,pricing,imagefamily}/provider.go` for the hit list.

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
docs/                          # bootstrap guides
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

# 4. Apply an HCloudNodeClass and NodePool (after running karpenter core itself).
kubectl apply -f examples/talos-nodeclass.yaml
```

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
