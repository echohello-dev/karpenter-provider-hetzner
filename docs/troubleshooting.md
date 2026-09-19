# Troubleshooting

Common failure modes, debug recipes, and the kubectl one-liners worth
remembering. Start here when a NodeClaim is stuck, an `HCloudNodeClass`
flaps, or a server comes up with the wrong image.

## Quick triage

```bash
# 1. Controller healthy?
kubectl -n karpenter get pods -l app.kubernetes.io/name=karpenter-provider-hetzner
kubectl -n karpenter logs -l app.kubernetes.io/name=karpenter-provider-hetzner --tail=200

# 2. NodeClass reconciled?
kubectl get hcloudnodeclass -o yaml | less

# 3. NodeClaim state and events?
kubectl describe nodeclaim <name> -n karpenter
kubectl get events -A --field-selector involvedObject.kind=NodeClaim --sort-by=.lastTimestamp

# 4. Hetzner side: do servers exist for this cluster?
hcloud server list -l karpenter.sh/cluster=$CLUSTER_NAME
```

## Controller won't start

### CrashLoopBackOff with `clusterName is required`

The chart refuses to render without `clusterName`. Set it explicitly:

```bash
helm upgrade karpenter-provider-hetzner \
  oci://ghcr.io/echohello-dev/charts/karpenter-provider-hetzner \
  --namespace karpenter \
  --reuse-values \
  --set clusterName=$CLUSTER_NAME
```

### CrashLoopBackOff on token lookup

```
secret "hcloud-token" not found
```

The `hcloud-token` Secret must exist in the controller's namespace
(default `karpenter`) **before** the pod starts, or the controller
fails to read the API token. Recreate:

```bash
kubectl -n karpenter create secret generic hcloud-token \
  --from-literal=token="$HCLOUD_TOKEN"
```

A `401 Unauthorized` from the hcloud API means the token exists but is
wrong-scoped or revoked. Rotate it in the Hetzner Cloud Console and
patch the Secret:

```bash
kubectl -n karpenter patch secret hcloud-token --type=json \
  -p='[{"op":"replace","path":"/data/token","value":"'$(echo -n "$NEW_TOKEN" | base64)'"}]'
```

The controller reads the Secret on every API call, so a restart is not
required.

## `HCloudNodeClass` not Ready

`kubectl describe hcloudnodeclass default` shows
`Reason=ImageSelectorResolutionFailed` (or similar).

### `status.resolvedImages` is empty

The selector resolved to zero snapshots. Verify:

```bash
# What does the hcloud API see?
hcloud image list \
  -l karpenter.sh/cluster=$CLUSTER_NAME \
  -o columns=id,description,architecture,status,type

# Or via curl with the same query the provider sends:
curl -s -H "Authorization: Bearer $HCLOUD_TOKEN" \
  "https://api.hetzner.cloud/v1/images?architecture=x86&type=snapshot&status=available&label_selector=caph-image-name=talos-v1.9.5-gvisor"
```

Common causes:

- **Snapshot isn't labelled.** `imageSelector.selector` requires the
  label to exist on the snapshot. Either set the label during image
  creation (via `caph`) or drop the selector and use `version` instead.
- **Wrong family.** `family: talos` won't match an Ubuntu snapshot even
  if the version string overlaps.
- **Wrong architecture.** The selector is filtered server-side by the
  NodeClaim's architecture requirement. Set
  `kubernetes.io/arch` on the NodePool.
- **Image is deprecated.** The controller skips deprecated and deleted
  images even if the API returns them — see
  [`pkg/providers/imagefamily`](../pkg/providers/imagefamily/provider.go).

### `HCloudNodeClass` `Ready=False, Reason=ReconcileFailed`

The reconciler hit an unrecoverable error. Read the message:

```bash
kubectl get hcloudnodeclass default -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

The most common cause is a missing or non-existent `networkID`. Validate:

```bash
hcloud network describe $NETWORK_ID
```

## NodeClaim stuck in `Launching`

```bash
kubectl describe nodeclaim <name> -n karpenter
```

Look at the `Message` field on the `Launched` condition.

### "no matching image for family=talos arch=x86"

The NodeClaim is requesting an arch the NodeClass has no image for.
`kubectl get hcloudnodeclass -o jsonpath='{.items[*].status.resolvedImages}'`
to see what was resolved; cross-reference with the NodePool's
`kubernetes.io/arch` requirement.

### "userData secret not found"

`userDataSecretRef.namespace` / `name` / `key` doesn't resolve to a real
Secret. Verify:

```bash
kubectl get secret -n karpenter talos-worker
kubectl get hcloudnodeclass default -o jsonpath='{.spec.userDataSecretRef}'
```

### "firewall <id> not found"

`firewallIDs` contains an ID the API can't see. List them:

```bash
hcloud firewall list -o columns=id,name
```

### Server created but never registers as a Node

The hcloud server exists but no `Node` appears. Walk through:

1. **`kubectl get events -A | grep -i nodeclaim`** — most provider-side
   failures surface here.
2. **`hcloud server describe <id>`** — confirm `status = running`.
3. **Talos API** (for Talos workers): from a control-plane node,
   `talosctl -n <public-ip> health` and `talosctl -n <public-ip> dmesg`.
   Common failure: firewall blocks TCP/50000 from worker subnet to
   control plane.
4. **cloud-init** (for Ubuntu workers): the userData is the cloud-init.
   `ssh ubuntu@<ip> sudo cloud-init status --long`.
5. **hcloud CCM installed?** The kubelet registers with
   `hcloud://<server-id>` providerIDs and the controller looks up
   servers by that ID. Without the CCM, the kubelet can't label the
   node correctly and `Get`/`List` return empty results. Install:

   ```bash
   helm repo add hcloud https://charts.hetzner.cloud
   helm install hcloud-cloud-controller-manager hcloud/hcloud-cloud-controller-manager \
     --namespace kube-system
   ```

## Servers appear but can't be deleted by Karpenter

Karpenter `Delete` finds servers by the `karpenter.sh/cluster` label. If
the controller is mis-configured with the wrong `clusterName`, it sees
zero servers and the NodeClaim hangs in `Deleting`. Confirm:

```bash
kubectl get deploy karpenter-provider-hetzner -n karpenter \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLUSTER_NAME")].value}'
```

It must match the label on the servers:

```bash
hcloud server list -o columns=id,name,labels
```

## Drift fires immediately after creation

`kubectl describe nodeclaim` shows `Drift=true`. The most common reasons:

- **`DriftImage`** — the snapshot resolved at create-time is no longer
  in `status.resolvedImages`. Usually a label was edited. Diff the
  current image against the selector:

  ```bash
  kubectl get hcloudnodeclass -o jsonpath='{.items[*].status.resolvedImages}'
  ```

- **`DriftNetwork`** — the server is on a different `networkID` than
  the NodeClass. Check `hcloud server describe <id>`.

- **`DriftLabels`** — server labels drifted from the NodeClass spec.
  This is almost always a manual `hcloud server update` or a second
  controller racing on the same cluster name. Don't share
  `clusterName` across Karpenter instances — see
  [Hetzner constraints](README.md#hetzner-constraints-apply-to-every-page-in-this-folder).

## Two clusters see each other's servers

You shared a Hetzner project across two clusters. Both controllers tag
servers with `karpenter.sh/cluster=<name>`, but if their `clusterName`
values collide, `List`/`Delete` see both sets. Rename one — there's no
way to migrate existing servers, so tear down and rebuild the affected
NodeClass.

## Getting billed for IPv4 on a private-network cluster

Set `enablePublicIPv4: false` on the NodeClass:

```bash
kubectl patch hcloudnodeclass default --type=merge \
  -p '{"spec":{"enablePublicIPv4":false}}'
```

Existing servers keep their public IPv4 until they're replaced — Karpenter
drift fires on the field change, and the new server comes up without
the address.

## Logs and metrics

- **Logs**: `kubectl -n karpenter logs -l app.kubernetes.io/name=karpenter-provider-hetzner`
  — controller logs are structured JSON with `cluster`, `nodepool`,
  `nodeclaim` fields populated on every line.
- **Metrics**: `kubectl -n karpenter port-forward svc/karpenter-provider-hetzner 8080:8080`,
  then `curl localhost:8080/metrics`. The controller emits Prometheus
  counters under
  [`pkg/metrics`](../pkg/metrics).
- **Events**: `kubectl get events -A --field-selector involvedObject.kind=NodeClaim`
  for the high-signal Karpenter lifecycle events.

## Reporting a bug

If the above recipes don't resolve it, capture before opening an issue:

```bash
kubectl -n karpenter logs -l app.kubernetes.io/name=karpenter-provider-hetzner --tail=1000 > provider.log
kubectl get hcloudnodeclass -o yaml > hcnc.yaml
kubectl get nodeclaims -A -o yaml > nodeclaims.yaml
kubectl get events -A --sort-by=.lastTimestamp > events.yaml
```

Also note:

- The Karpenter core version (`helm list -n karpenter -o yaml`).
- The hcloud-go SDK version (`go list -m github.com/hetznercloud/hcloud-go/v2`).
- The provider chart version (`helm list -n karpenter -o yaml`).
- The cluster name (sanitized — never paste the hcloud token).
