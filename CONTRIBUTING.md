# Contributing

Thanks for your interest in making `karpenter-provider-hetzner` better.

## Development

```bash
mise trust && mise install
mise run ci       # lint, vet, test, build, generate-verify
```

The controller-gen marker generator must agree with the source Go file
contents: every release CI job runs `mise run generate-verify`, which
diff-checks the outputs. If you change `pkg/apis/v1/hcloudnodeclass_types.go`,
run `mise run generate` and commit the resulting
`pkg/apis/v1/zz_generated.deepcopy.go` and the matching CRD YAML under
`charts/karpenter-provider-hetzner/crds/`.

## Pull requests

1. Branch from `main`.
2. Keep commits small and conventional (`feat:`, `fix:`, `chore:`, `docs:`).
3. Include a regression test alongside bug fixes.
4. Note when a hcloud API call would require a live Hetzner project — that
   is out of scope for the unit-test lane and belongs in the e2e suite.

## Security disclosures

See `SECURITY.md`. Do not open a public issue for a vulnerability.

## Code of conduct

By participating you agree to the rules in `CODE_OF_CONDUCT.md`.
