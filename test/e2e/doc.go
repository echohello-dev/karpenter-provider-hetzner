// Package e2e holds the end-to-end scenario suite for
// karpenter-provider-hetzner.
//
// The default path is fully local: controller-runtime envtest runs a real
// kube-apiserver with the CRDs from charts/karpenter-provider-hetzner/crds,
// the real HCloudNodeClass reconciler is driven through a real manager, and
// the real CloudProvider round-trips against a stateful httptest Hetzner Cloud
// fake (see fakehcloud). Tests skip cleanly when the envtest binaries are
// absent (KUBEBUILDER_ASSETS unset), so `go test ./...` stays green without
// any setup.
//
// An opt-in smoke path against the live Hetzner Cloud API lives in
// real_hetzner_test.go and only runs when E2E=1 and HCLOUD_TOKEN are both set.
package e2e
