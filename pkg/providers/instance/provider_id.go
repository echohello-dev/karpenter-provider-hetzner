package instance

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatProviderID formats a Hetzner server ID to the providerID string
// Karpenter and the hcloud-ccm use: "hcloud://<id>".
func FormatProviderID(id int64) string {
	return providerPrefix + strconv.FormatInt(id, 10)
}

// ParseProviderID reverses FormatProviderID. Returns the integer server ID
// when the prefix matches; otherwise returns an error so callers never
// silently treat a malformed string as "not found".
func ParseProviderID(providerID string) (int64, string, error) {
	if !strings.HasPrefix(providerID, providerPrefix) {
		return 0, "", fmt.Errorf("instance: provider ID %q does not start with %q", providerID, providerPrefix)
	}
	raw := strings.TrimPrefix(providerID, providerPrefix)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("instance: provider ID %q has non-integer server ID %q: %w", providerID, raw, err)
	}
	return id, raw, nil
}

const providerPrefix = "hcloud://"
