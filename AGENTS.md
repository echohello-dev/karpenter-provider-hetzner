# Agent Skills

This repository follows the Agent Skills specification.

Specifications:

- [Agent Skills Specification](https://agentskills.io/specification.md)
- [LLM Interaction Guide](https://agentskills.io/llms.txt)

## Common Tasks

See `mise.toml` for defined tasks. The standard tasks for this provider are:

| Task                       | Command             |
|---|---|
| Build                      | `mise run build`    |
| Unit tests                 | `mise run test`     |
| Lint                       | `mise run lint`     |
| Vet                        | `mise run vet`      |
| Generate CRDs              | `mise run generate` |
| Verify generated files     | `mise run generate-verify` |
| Run all CI checks          | `mise run ci`       |
| Lint the Helm chart        | `mise run lint-chart` |
| Cut a release              | `mise run release`  |

## Hetzner Constraints

- Every managed Hetzner server carries the `karpenter.sh/cluster=<CLUSTER_NAME>` label. `LIST`/`DELETE` operations are scoped by that label so two clusters sharing one Hetzner project cannot see each other's servers. The controller fails fast if `CLUSTER_NAME` is empty.
- Karpenter does not have a Hetzner spot market, so every instance is `capacity-type=on-demand`. The Instancetype label is always hard-coded.
- Hetzner bills the primary IPv4 separately. On private-network clusters set `enablePublicIPv4: false` to drop the cost.

## Repo Conventions

- Public types live in `pkg/apis/v1`. Generated code is at `zz_generated.deepcopy.go`; the matching CRDs go under `charts/karpenter-provider-hetzner/crds/`.
- Drift reasons are defined as package constants in `pkg/cloudprovider/cloudprovider.go` (`DriftImage`, `DriftNetwork`, ...). Add new ones in the same place.
- Methods on `*hcloud.Client` are isolated to provider sub-packages. The cloudprovider orchestrates them, never touches hcloud directly.
