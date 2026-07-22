// Package version provides the single source of truth for the provider's
// release version. The `var Version` is overridden at build time via
// `-ldflags "-X github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/version.Version=v0.1.0"`.
// When unoverridden it returns "dev", so non-release builds report something
// obviously non-versioned.
package version

// Version is the provider version. Overridden at build/link time by goreleaser.
var Version = "dev"
