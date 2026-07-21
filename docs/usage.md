# Releasing karpenter-provider-hetzner

## Bootstrap

1. Have a [Hetzner Cloud](https://console.hetzner.cloud) project with API
   read+write token.
2. Bring up a Talos Kubernetes cluster on Hetzner. Use [hcloud-k8s/terraform-hcloud-kubernetes](https://github.com/hcloud-k8s/terraform-hcloud-kubernetes)
   or [kube-hetzner](https://github.com/kube-hetzner/kube-hetzner).
3. Apply a Talos worker bootstrap machineconfig into a Kubernetes Secret,
   e.g. via `talosctl gen secrets`/`talosctl gen config` then
   `kubectl create secret generic talos-worker -n karpenter --from-file=userData=worker.yaml`.
4. Install the controller with the OCI Helm chart, supplying the cluster
   name and pointing at the Secret holding your hcloud API token.

The full recipe lives in `docs/talos-bootstrap.md`.

## Quick install

```bash
helm repo add karpenter-hetzner https://echohello-dev.github.io/karpenter-provider-hetzner
helm install karpenter-provider-hetzner \
  karpenter-hetzner/karpenter-provider-hetzner \
  --version vX.Y.Z \
  --namespace karpenter \
  --create-namespace \
  --set clusterName=$CLUSTER_NAME

kubectl -n karpenter create secret generic hcloud-token \
  --from-literal=token=$HCLOUD_TOKEN
```

After the controller is up, apply the example `HCloudNodeClass` and
`NodePool` from `examples/`.
