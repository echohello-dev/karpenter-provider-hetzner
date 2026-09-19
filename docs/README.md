# karpenter-provider-hetzner operator docs

| Doc | Covers |
|---|---|
| [`install.md`](install.md) | Prerequisites, Helm install, post-install verification |
| [`configuration.md`](configuration.md) | `HCloudNodeClass` spec reference (every field) |
| [`troubleshooting.md`](troubleshooting.md) | Common failures, debug recipes, log/event queries |
| [`talos-bootstrap.md`](talos-bootstrap.md) | Bootstrapping Talos workers (machineconfig + Secret) |
| [`ubuntu-bootstrap.md`](ubuntu-bootstrap.md) | Bootstrapping Ubuntu workers (cloud-init + kubeadm join) |
| [`usage.md`](usage.md) | One-page summary, links to the above |

## Hetzner constraints (apply to every page in this folder)

- **`clusterName` is mandatory.** Every Hetzner server the controller creates is stamped with `karpenter.sh/cluster=<CLUSTER_NAME>`. `LIST` and `DELETE` operations are scoped by that label, so two clusters sharing one Hetzner project cannot see each other's servers. The chart fails to render if `clusterName` is empty.
- **No spot market.** Karpenter does not have a Hetzner spot market, so every NodeClaim is `capacity-type=on-demand`. The `karpenter.sh/capacity-type` requirement must be `on-demand`.
- **Primary IPv4 is billed separately.** On private-network clusters, set `enablePublicIPv4: false` on the NodeClass to drop the per-server IPv4 charge.
- **Hetzner Cloud Controller Manager (hcloud CCM) is required.** Kubelets register with `hcloud://<server-id>` providerIDs; the controller uses that ID to reconcile `Delete`/`Get`/`List`.
