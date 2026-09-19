# `HCloudNodeClass` configuration reference

`HCloudNodeClass` is the per-cluster type definition a `NodePool`
references. It tells the controller:

- which Hetzner location(s) to schedule into,
- which private network, firewalls, and SSH keys to attach,
- which snapshot to boot from, and
- the worker `userData` to pass to Hetzner as cloud-init / Talos
  machineconfig.

Every required field below maps directly to a Hetzner Cloud API concept
— see [the Hetzner API docs](https://docs.hetzner.cloud/) for the
underlying resource definitions.

## Full example

```yaml
apiVersion: karpenter.hetzner.cloud/v1
kind: HCloudNodeClass
metadata:
  name: default
spec:
  # Required: at least one.
  locations:
    - nbg1
    - fsn1

  # Required: ID of the private network new servers attach to.
  # Get with: hcloud network list -o columns=id,name
  networkID: 1234567

  imageSelector:
    family: talos           # Required: 'talos' or 'ubuntu'.
    selector:               # Optional: pin an exact snapshot by label.
      caph-image-name: talos-v1.9.5-gvisor
    version: "v1.9"         # Optional: substring match against description.

  firewallIDs: [1234567]    # Optional
  sshKeyIDs: []             # Optional
  placementGroupStrategy: spread   # 'spread' (default) or 'none'.
  enablePublicIPv4: false   # Set false on private-network clusters.
  enablePublicIPv6: false   # Set false on private-network clusters.

  labels:                   # Optional: extra labels on the hcloud server.
    team: backend

  # Sourced from a Secret (preferred). Required: at least one of
  # userData or userDataSecretRef.
  userDataSecretRef:
    namespace: karpenter
    name: talos-worker
    key: userData
```

## Spec field reference

### `locations` *(required, []string)*

Hetzner locations the NodeClass may schedule into (e.g. `nbg1`, `fsn1`,
`hel1`, `ash`, `hil`). Must contain at least one element — this is a
kubebuilder `minItems: 1` validation. Karpenter picks the cheapest
location in this list that has capacity for the requested server type.

```yaml
spec:
  locations: [nbg1, fsn1]
```

### `networkID` *(required, int64)*

ID of the Hetzner private network the server attaches to. Set this even
on public-only clusters — most Karpenter deployments want the kubelet on
a private subnet for security. Get the ID with `hcloud network list`.

```yaml
spec:
  networkID: 1234567
```

### `imageSelector` *(required, object)*

Picks the snapshot to boot from. Always resolved server-side then
filtered client-side; see
[`pkg/providers/imagefamily`](../pkg/providers/imagefamily/provider.go)
for the matching rules.

#### `imageSelector.family` *(required, string)*

OS family. **Only `talos` and `ubuntu` are supported** — this is a
kubebuilder `XValidation` rule and the API rejects other values. Talos is
recommended; see [`talos-bootstrap.md`](talos-bootstrap.md) for the
machineconfig story and [`ubuntu-bootstrap.md`](ubuntu-bootstrap.md) for
the cloud-init story.

#### `imageSelector.selector` *(optional, map[string]string)*

Exact-match hcloud label selector (e.g.
`{"caph-image-name": "talos-v1.9.5-gvisor"}`). **Prefer this over
`version`** when you need reproducibility across image rebuilds. All keys
must match.

#### `imageSelector.version` *(optional, string)*

Case-insensitive substring match against the snapshot description (e.g.
`"v1.9"` or `"24.04"`). When omitted, the newest matching snapshot wins.
Ignored when `selector` is set.

### `firewallIDs` *(optional, []int64)*

Hetzner firewalls to attach to the server. Most clusters only need one
(private-network + nodeport range); keep it minimal.

### `sshKeyIDs` *(optional, []int64)*

SSH keys injected on the server. Useful for Ubuntu workers; not needed
for Talos.

### `placementGroupStrategy` *(optional, enum: `spread` | `none`, default `spread`)*

Whether to spread servers across physical hosts. Defaults to `spread` —
keep this on unless you have a reason to colocate (rare). The chosen
placement group is reported in `status.selectedPlacementGroup`.

### `enablePublicIPv4` *(optional, bool, default `true`)*

Whether to attach a primary public IPv4 to each server. **Set false on
private-network clusters** to drop the per-server IPv4 charge — see
[`AGENTS.md`](../AGENTS.md) for the constraint rationale.

### `enablePublicIPv6` *(optional, bool, default `true`)*

Whether to attach a public IPv6. Drop on private-network clusters for
the same reason as `enablePublicIPv4`.

### `labels` *(optional, map[string]string)*

Extra hcloud labels applied to every server the NodeClass creates.
Karpenter-managed labels (`karpenter.sh/cluster`, `karpenter.sh/nodepool`)
are added automatically — don't duplicate them here.

### `userData` and `userDataSecretRef`

Exactly one of `userData` (inline) or `userDataSecretRef` (sourced from
a Secret) must be set. **Prefer `userDataSecretRef`** — keeping
machineconfig / cloud-init in git is a foot-gun (it almost always
contains tokens or CAs you don't want committed).

#### `userData` *(optional, string)*

Inline Talos machineconfig or cloud-init YAML. The controller passes the
literal value to Hetzner as `user_data` — no template substitution.

#### `userDataSecretRef` *(optional, object)*

```yaml
spec:
  userDataSecretRef:
    namespace: karpenter  # Required.
    name: talos-worker    # Required.
    key: userData         # Required.
```

The Secret is read at NodeClaim create-time only. Rotating the Secret
value does **not** rotate the value on already-provisioned servers —
Karpenter has to replace the server for that.

## Status reference

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: Reconciled
      message: "Resolved amd64 image 12345678"
      lastTransitionTime: "..."
      observedGeneration: 1
  resolvedImages:
    - architecture: amd64
      imageID: 12345678
    - architecture: arm64
      imageID: 12345679
  selectedPlacementGroup: spread-abc123
```

| Field | Meaning |
|---|---|
| `status.conditions[]` | Standard Kubernetes conditions. Look at `type=Ready` for the headline signal. |
| `status.resolvedImages[]` | Image IDs the controller resolved per architecture. Empty when no image matches the selector. |
| `status.selectedPlacementGroup` | Hetzner placement group chosen by the NodeClass (only set when `placementGroupStrategy = spread`). |

## Validation rules (kubebuilder, baked into the CRD)

- `spec.imageSelector.family` must be exactly `"talos"` or `"ubuntu"`.
- `spec.locations` must contain at least one element.
- `spec.imageSelector`, `spec.locations`, `spec.networkID` are all required.

These are enforced at admission — `kubectl apply` of an invalid
NodeClass is rejected before the controller ever sees it.

## Drift

The cloudprovider computes drift against the live Hetzner state and
flags NodeClaims for replacement when:

- the resolved image no longer matches the selector,
- the server is attached to a different network,
- the labels on the server diverged from the NodeClass spec,
- or the server is on a wrong / deprecated architecture.

Drift reasons are constants in
[`pkg/cloudprovider/cloudprovider.go`](../pkg/cloudprovider/cloudprovider.go)
(`DriftImage`, `DriftNetwork`, ...).
