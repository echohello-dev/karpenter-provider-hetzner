package instancetype

import (
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestClassOf(t *testing.T) {
	cases := []struct {
		name string
		st   *hcloud.ServerType
		want Family
	}{
		{"empty", &hcloud.ServerType{Name: ""}, FamilyOther},
		{"nil", nil, FamilyOther},
		{"cx22", &hcloud.ServerType{Name: "cx22"}, FamilyCX},
		{"cpx42", &hcloud.ServerType{Name: "cpx42"}, FamilyCPX},
		{"ccx13", &hcloud.ServerType{Name: "ccx13"}, FamilyCCX},
		{"cax21", &hcloud.ServerType{Name: "cax21"}, FamilyCAX},
		{"unknown", &hcloud.ServerType{Name: "fake-1"}, FamilyOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassOf(tc.st); got != tc.want {
				var name string
				if tc.st != nil {
					name = tc.st.Name
				}
				t.Fatalf("ClassOf(%q) = %q, want %q", name, got, tc.want)
			}
		})
	}
}
