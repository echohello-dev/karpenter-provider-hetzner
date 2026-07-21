# Bootstrapping Talos workers

This provider accepts a Talos worker machineconfig as `userData` on the
NodeClass and provisions Hetzner Cloud servers that boot straight into the
cluster.

## Generate the worker machineconfig

From a directory holding your `talosconfig`:

```bash
talosctl gen config my-cluster https://kube.example.com:6443 \
  --output-dir _out
```

This produces:

- `_out/controlplane.yaml` — control-plane machineconfig (already used by
  your bootstrap).
- `_out/worker.yaml` — worker machineconfig; this is what the NodeClass
  should reference.

## Pluck the worker join token

```bash
cat _out/talosconfig > /dev/null   # generate secret in _out
SECRET=$(talosctl get secrets -n kube-system -o yaml | yq '.spec.token')
```

## Push the worker machineconfig into a Secret

```bash
kubectl -n karpenter create secret generic talos-worker \
  --from-file=userData=_out/worker.yaml
```

## Reference the Secret from the NodeClass

```yaml
apiVersion: karpenter.hetzner.cloud/v1
kind: HCloudNodeClass
metadata:
  name: default
spec:
  locations: [nbg1]
  networkID: 12345
  imageSelector:
    family: talos
    selector:
      caph-image-name: talos-v1.9.5-gvisor
  userDataSecretRef:
    namespace: karpenter
    name: talos-worker
    key: userData
```

When a NodeClaim is created the controller reads the Secret at create-time
and passes the machineconfig through to the Hetzner server as `userData`.
The value never lands in the NodeClass spec, status, or git.

## Pin the image

Pin the snapshot with the `caph-image-name` label so a stale snapshot
sitting in your project doesn't take precedence over the version + baked
extensions you trust.

## Verify the worker joined

```bash
kubectl wait --for=condition=Ready nodes --all --timeout=5m
kubectl get nodes -o wide
```

If the worker fails to register, check:

1. The Talos API is reachable from the worker — i.e. the firewall on the
   control-plane nodes allows TCP/50000 from the worker's subnet.
2. The Secret holds a valid worker machineconfig (`talosctl validate -f
   worker.yaml`).
3. The image selector resolved to a snapshot matching the worker's
   architecture (`kubectl get hcloudnodeclass -o yaml`).
