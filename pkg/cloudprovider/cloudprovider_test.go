package cloudprovider

import (
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestResolveArchitecture(t *testing.T) {
	cases := []struct {
		arch string
		want hcloud.Architecture
	}{
		{"arm64", hcloud.ArchitectureARM},
		{"amd64", hcloud.ArchitectureX86},
		{"", hcloud.ArchitectureX86}, // empty => default to x86 (matches upstream behaviour)
		{"other", hcloud.ArchitectureX86},
	}
	for _, tc := range cases {
		t.Run(tc.arch, func(t *testing.T) {
			if got := resolveArchitecture(tc.arch); got != tc.want {
				t.Fatalf("resolveArchitecture(%q) = %v, want %v", tc.arch, got, tc.want)
			}
		})
	}
}
