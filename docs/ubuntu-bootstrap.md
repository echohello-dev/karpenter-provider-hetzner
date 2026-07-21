# Bootstrapping Ubuntu workers

For clusters running kubeadm on Ubuntu instead of Talos, build a
cloud-init `userData` that joins the worker to the existing control plane
on first boot, and reference it from the NodeClass.

## cloud-init skeleton

```yaml
#cloud-config
write_files:
  - path: /etc/kubernetes/kubelet-bootstrap-kubeconfig
    permissions: '0644'
    content: |
      apiVersion: v1
      kind: Config
      clusters:
      - name: my-cluster
        cluster:
          server: https://kube.example.com:6443
          certificate-authority-data: <BASE64_CA_BUNDLE>
      contexts:
      - name: bootstrap-token-context
        context:
          cluster: my-cluster
          user: kubelet-bootstrap
      users:
      - name: kubelet-bootstrap
        user:
          token: <BOOTSTRAP_TOKEN>
      current-context: bootstrap-token-context
runcmd:
  - apt-get update
  - apt-get install -y apt-transport-https
  - curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.35/deb/Releases.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  - echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.35/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  - apt-get install -y kubelet kubeadm kubectl
  - apt-mark hold kubelet kubeadm kubectl
  - kubeadm join --config /etc/kubernetes/kubelet-bootstrap-kubeconfig --ignore-preflight-errors=Swap
```

## Pluck the kube-public CA + bootstrap token

After `kubeadm init` on the first control-plane node:

```bash
# CA bundle (base64, no PEM headers)
kubectl get cm kube-public -n kube-public -o jsonpath='{.data.ca\.crt}' | base64 -w0
# Bootstrap token (output from `kubeadm token create --print-join-command`)
```

## Push the cloud-init into a Secret

Avoid keeping a real bootstrap token in git:

```bash
kubectl -n karpenter create secret generic ubuntu-worker \
  --from-file=userData=./ubuntu-worker.yaml
```

## Reference it from the NodeClass

```yaml
apiVersion: karpenter.hetzner.cloud/v1
kind: HCloudNodeClass
metadata:
  name: ubuntu
spec:
  locations: [nbg1]
  networkID: 12345
  imageSelector:
    family: ubuntu
    version: "24.04"
  userDataSecretRef:
    namespace: karpenter
    name: ubuntu-worker
    key: userData
  placementGroupStrategy: spread
  enablePublicIPv4: false
```

Caveats:

- A kubeadm join involves a short-lived bootstrap token. Rotate the
  Secret, or have your NodeClass mutate to drop the field.
- The hcloud Cloud Controller Manager must be installed in the cluster so
  nodes get their `hcloud://` providerIDs — Karpenter relies on them for
  `Delete`/`Get`/`List`.

## Trade-off vs Talos

Talos is the recommended path because the machineconfig is declarative and
the boot is reproducible across image upgrades. The Ubuntu path is the
right choice when a non-Kubernetes host agent or a custom CNI needs an
OS-level scaffolding kubeadm does not provide.
