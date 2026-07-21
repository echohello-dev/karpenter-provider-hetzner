package v1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlacementGroupStrategy controls how nodes are spread across Hetzner physical hosts.
// +enum
type PlacementGroupStrategy string

const (
	PlacementGroupSpread PlacementGroupStrategy = "spread"
	PlacementGroupNone   PlacementGroupStrategy = "none"
)

// ImageFamily is the OS image family the NodeClass should boot from.
// +enum
type ImageFamily string

const (
	ImageFamilyTalos  ImageFamily = "talos"
	ImageFamilyUbuntu ImageFamily = "ubuntu"
)

// ImageSelector describes how to select a Hetzner snapshot to boot from.
type ImageSelector struct {
	// Family selects the OS family (talos|ubuntu).
	Family ImageFamily `json:"family"`
	// Version is an optional substring match against the image description
	// (e.g. "v1.9"). Defaults to the newest matching image.
	// +optional
	Version string `json:"version,omitempty"`
	// Selector is an optional hcloud-label filter (e.g.
	// {"caph-image-name": "talos-v1.9.5-gvisor"}). All labels must match.
	// Prefer this over Version when pinning an exact snapshot.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`
}

// UserDataSecretRef references a Kubernetes Secret whose value is the
// user-data blob passed to the Hetzner server at create time (Talos machine
// config or Ubuntu cloud-init). When set, takes precedence over inline
// Spec.UserData.
type UserDataSecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// ResolvedImage records an image ID that imagefamily.Provider resolved for a
// NodeClass, keyed by architecture (amd64 | arm64).
type ResolvedImage struct {
	Architecture string `json:"architecture"`
	ImageID      int64  `json:"imageID"`
}

// HCloudNodeClassSpec defines the desired state of HCloudNodeClass.
//
// +kubebuilder:validation:XValidation:rule="self.locations.size() > 0",message="locations must contain at least one element"
type HCloudNodeClassSpec struct {
	// Locations is the list of Hetzner locations nodes may be scheduled into.
	// +kubebuilder:validation:MinItems=1
	Locations []string `json:"locations"`
	// NetworkID is the private network ID nodes should attach to.
	// Required for clusters that place worker traffic on a private network.
	// +kubebuilder:validation:Required
	NetworkID int64 `json:"networkID"`
	// ImageSelector selects the snapshot to boot from.
	// +kubebuilder:validation:XValidation:rule="(has(self.family) && (self.family == 'talos' || self.family == 'ubuntu'))",message="family must be 'talos' or 'ubuntu'"
	ImageSelector ImageSelector `json:"imageSelector"`
	// FirewallIDs is the optional list of Hetzner firewalls to attach.
	// +optional
	FirewallIDs []int64 `json:"firewallIDs,omitempty"`
	// SSHKeyIDs is the optional list of SSH keys to install on the server.
	// +optional
	SSHKeyIDs []int64 `json:"sshKeyIDs,omitempty"`
	// Labels is the set of labels applied to the Hetzner server at create time.
	// Karpenter-managed labels (karpenter.sh/cluster, karpenter.sh/nodepool)
	// are applied automatically and do not need to be specified here.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// PlacementGroupStrategy controls whether nodes are spread across physical hosts.
	// Defaults to spread.
	// +kubebuilder:validation:Enum=spread;none
	// +optional
	PlacementGroupStrategy PlacementGroupStrategy `json:"placementGroupStrategy,omitempty"`
	// EnablePublicIPv4 attaches a billed public IPv4 to each server. Set false
	// on private-network clusters to drop the IPv4 charge.
	// +kubebuilder:default=true
	// +optional
	EnablePublicIPv4 *bool `json:"enablePublicIPv4,omitempty"`
	// EnablePublicIPv6 attaches a public IPv6 to each server.
	// +kubebuilder:default=true
	// +optional
	EnablePublicIPv6 *bool `json:"enablePublicIPv6,omitempty"`
	// UserData is the inline cloud-init (Ubuntu) or Talos machine config blob.
	// Prefer UserDataSecretRef — keeping boot config in git is a foot-gun.
	// +optional
	UserData string `json:"userData,omitempty"`
	// UserDataSecretRef sources userData from a Kubernetes Secret. Takes
	// precedence over Spec.UserData when both are set.
	// +optional
	UserDataSecretRef *UserDataSecretRef `json:"userDataSecretRef,omitempty"`
}

// HCloudNodeClassStatus defines the observed state of HCloudNodeClass.
type HCloudNodeClassStatus struct {
	// Conditions are the readiness signals reported by the controller.
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
	// ResolvedImages is the set of image IDs the provider resolved per architecture.
	// +optional
	ResolvedImages []ResolvedImage `json:"resolvedImages,omitempty"`
	// SelectedPlacementGroup is the placement group the NodeClass chose for its
	// servers (when PlacementGroupStrategy = spread).
	// +optional
	SelectedPlacementGroup string `json:"selectedPlacementGroup,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=hcloudnodeclasses,shortName=hcnodeclass,singular=hcloudnodeclass

// HCloudNodeClass is the Schema for the NodeClass used by Karpenter to know
// how a Hetzner Cloud server should be built when a NodeClaim references it.
type HCloudNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HCloudNodeClassSpec   `json:"spec,omitempty"`
	Status HCloudNodeClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HCloudNodeClassList contains a list of HCloudNodeClass.
type HCloudNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HCloudNodeClass `json:"items"`
}

// GetConditions returns the readiness conditions reported on the NodeClass.
func (in *HCloudNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

// SetConditions replaces the readiness conditions reported on the NodeClass.
func (in *HCloudNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

// StatusConditions returns a ConditionSet view of the NodeClass's status
// conditions.
func (in *HCloudNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return conditionTypes.For(in, opts...)
}

// PublicIPv4Enabled returns whether the NodeClass should attach a public IPv4.
func (s HCloudNodeClassSpec) PublicIPv4Enabled() bool {
	return s.EnablePublicIPv4 == nil || *s.EnablePublicIPv4
}

// PublicIPv6Enabled returns whether the NodeClass should attach a public IPv6.
func (s HCloudNodeClassSpec) PublicIPv6Enabled() bool {
	return s.EnablePublicIPv6 == nil || *s.EnablePublicIPv6
}

// Aggregated sub-condition types the controller reconciles. Aggregated
// ConditionTypeReady is True only when all of these are True.
const (
	ConditionTypeImagesReady    = "ImagesReady"
	ConditionTypeNetworkReady   = "NetworkReady"
	ConditionTypeResourcesReady = "ResourcesReady"
	ConditionTypeUserDataReady  = "UserDataReady"
)

var conditionTypes = status.NewReadyConditions(
	ConditionTypeImagesReady,
	ConditionTypeNetworkReady,
	ConditionTypeResourcesReady,
	ConditionTypeUserDataReady,
)

func init() {
	SchemeBuilder.Register(&HCloudNodeClass{}, &HCloudNodeClassList{})
}
