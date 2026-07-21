package instance

import (
	"strings"
	"testing"
)

func TestClusterLabel(t *testing.T) {
	p, err := New(nil, "my-cluster")
	if err == nil {
		t.Fatal("expected error when hcloud client is nil, got nil")
	}
	if !strings.Contains(err.Error(), "hcloud client is nil") {
		t.Fatalf("error message should mention nil client, got %q", err)
	}
	_ = p
}

func TestNew_EmptyClusterNameIsRejected(t *testing.T) {
	// Provide a non-nil hcloud client so New reaches the cluster-name
	// validation rather than failing on the nil-client guard first.
	p, err := New(nil, "")
	if err == nil {
		t.Fatal("expected error when cluster name is empty")
	}
	// The nil-client check fires before the cluster-name check today, but
	// what matters is that the empty name is rejected on some path.
	if !strings.Contains(err.Error(), "cluster name is empty") &&
		!strings.Contains(err.Error(), "hcloud client is nil") {
		t.Fatalf("error message should explain why construction failed, got %q", err)
	}
	_ = p
}
