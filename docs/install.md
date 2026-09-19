# Install

End-to-end install of `karpenter-provider-hetzner` on an existing Hetzner
Cloud Kubernetes cluster.

## Prerequisites

- **Kubernetes cluster on Hetzner Cloud**, control-plane reachable from
  new workers. Recommended: bring-up with
  [`hcloud-k8s/terraform-hcloud-kubernetes`](https://github.com/hcloud-k8s/terraform-hcloud-kubernetes)
  or [`kube-hetzner`](https://github.com/kube-hetzner/kube-hetzner).
- **Karpenter v1** core installed in the cluster (this provider is a
  *cloud provider plugin*, not a replacement for the core). Install via
  the upstream
  [`karpenter`](https://helm.sh/docs/charts/karpenter-oci/karpenter)
  chart first.
- **[`hcloud-cloud-controller-manager`](https://github.com/hetznercloud/hcloud-cloud-controller-manager)**
  installed so worker nodes register with `hcloud://<id>` providerIDs.
  The controller needs those IDs to `Get`/`List`/`Delete` servers.
- **Hetzner Cloud API token** with **Read & Write** on the project.
  Generate at *Hetzner Cloud Console → Project → Security → API Tokens*.
- A **cluster name** that is unique within the Hetzner project. The
  provider uses it as a label on every server it creates; collisions
  cause servers from one cluster to be visible to another.

## 1. Install Karpenter (core)

```bash
helm repo add karpenter https://helm.sh/docs/charts/karpenter-oci/karpenter
helm install karpenter karpenter/karpenter \
  --namespace karpenter --create-namespace \
  --set settings.clusterName=$CLUSTER_NAME
```

Capture the Karpenter version — the provider is built against the same
upstream release (currently Karpenter v1.14).

## 2. Install the Hetzner provider

```bash
helm install karpenter-provider-hetzner \
  oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner \
  --version vX.Y.Z \
  --namespace karpenter \
  --set clusterName=$CLUSTER_NAME
```

Or pin to a values file:

```bash
helm install karpenter-provider-hetzner \
  oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner \
  --version vX.Y.Z \
  --namespace karpenter \
  -f values.yaml
```

`clusterName` is **required** — the chart fails to render without it.

### Required values

| Key | Description |
|---|---|
| `clusterName` | Unique cluster name. Stamped on every managed server as `karpenter.sh/cluster=<name>`. |
| `auth.secretRef.name` | Name of the Secret holding the hcloud API token. Defaults to `hcloud-token`. |
| `auth.secretRef.key` | Key in the Secret that holds the token. Defaults to `token`. |

### Recommended values for private-network clusters

```yaml
clusterName: my-cluster
auth:
  secretRef:
    name: hcloud-token
    key: token
```

## 3. Create the hcloud token Secret

```bash
kubectl -n karpenter create secret generic hcloud-token \
  --from-literal=token="$HCLOUD_TOKEN"
```

Verify:

```bash
kubectl -n karpenter get secret hcloud-token
```

The controller reads this Secret at startup and on every API call — it
never copies the token into the NodeClass or any other resource.

## 4. Verify the controller is up

```bash
kubectl -n karpenter get deploy karpenter-provider-hetzner
kubectl -n karpenter logs -l app.kubernetes.io/name=karpenter-provider-hetzner
```

The pod should log a successful leader-election lock within ~10 s of
start. If leader election never resolves, see
[`troubleshooting.md`](troubleshooting.md).

Confirm the CRDs landed:

```bash
kubectl get crd | grep -E 'hcloudnodeclasses|nodepools|nodeclaims|nodeoverlays|capacitybuffers'
```

Expected:

```
autoscaling.x-k8s.io_capacitybuffers.yaml
karpenter.hetzner.cloud_hcloudnodeclasses.yaml
karpenter.sh_nodeclaims.yaml
karpenter.sh_nodeoverlays.yaml
karpenter.sh_nodepools.yaml
```

## 5. Apply a NodeClass and a NodePool

Start from the bundled examples:

```bash
kubectl apply -f examples/secret.yaml          # hcloud-token (if you didn't create it earlier)
kubectl apply -f examples/talos-nodeclass.yaml # HCloudNodeClass named "default"
kubectl apply -f examples/nodepool.yaml        # NodePool "small-arm-only"
```

The `HCloudNodeClass` reconciler immediately lists images in the listed
locations and writes `status.resolvedImages`. Confirm:

```bash
kubectl get hcloudnodeclass default -o yaml
```

You should see a `status.conditions` entry of type `Ready = True` and at
least one entry in `status.resolvedImages`. If not, see
[`troubleshooting.md`](troubleshooting.md#hcloudnodeclass-not-ready).

## 6. Trigger a workload to force a NodeClaim

```bash
kubectl run nginx --image=nginx --rm -it --restart=Never --requests='cpu=500m,memory=512Mi'
```

Watch a node come up:

```bash
kubectl get nodeclaims -A -w
kubectl get nodes -w
```

The new node should appear in ~30–60 s (Hetzner server create + Talos
boot + kubelet registration).

## Upgrades

The provider ships its own CRDs. Helm handles them via the
`crds/install-crds.sh`-equivalent hook baked into the chart, so a normal
`helm upgrade` picks up API changes:

```bash
helm upgrade karpenter-provider-hetzner \
  oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner \
  --version vX.Y.Z \
  --namespace karpenter
```

Check the [CHANGELOG](../CHANGELOG.md) before upgrading across minor
versions — `HCloudNodeClass` is `v1` but the NodePool spec comes from
upstream Karpenter and may have changed.

## Uninstall

```bash
helm uninstall karpenter-provider-hetzner -n karpenter
```

> **Note:** Helm will not delete the CRDs by default. If you want a clean
> uninstall, pass `--set crds.keep=false` (chart default) and manually
> `kubectl delete crd` the five CRDs listed above. Servers and
> `HCloudNodeClass` resources will be left in place — clean them up
> separately.
