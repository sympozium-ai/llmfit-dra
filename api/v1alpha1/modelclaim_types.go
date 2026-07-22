package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types written by the ModelClaim controller.
const (
	// ConditionResolved reports whether spec.model resolved against llmfit's
	// database and the physics bounds were derived.
	ConditionResolved = "Resolved"
	// ConditionSatisfiable reports whether any published device satisfies the
	// resolved bounds. Advisory only — never blocks template creation.
	ConditionSatisfiable = "Satisfiable"
)

// Condition reasons written by the ModelClaim controller.
const (
	ReasonResolved         = "Resolved"
	ReasonInvalidModel     = "InvalidModel"
	ReasonResolveFailed    = "ResolveFailed"
	ReasonTemplateConflict = "TemplateConflict"
	ReasonDevicesAvailable = "DevicesAvailable"
	ReasonAllDevicesHeld   = "AllDevicesHeld"
	ReasonNoCandidates     = "NoCandidates"
)

// ModelClaimSpec names a model and a throughput target; the controller
// resolves the physics and materializes a same-named ResourceClaimTemplate.
// +kubebuilder:validation:XValidation:rule="!has(self.minTps) || self.minTps > 0.0",message="minTps must be > 0"
// +kubebuilder:validation:XValidation:rule="!has(self.efficiencyPct) || (self.efficiencyPct >= 1 && self.efficiencyPct <= 100)",message="efficiencyPct must be in 1..100"
type ModelClaimSpec struct {
	// Catalog name or HuggingFace repo id, resolved against the llmfit model
	// database (embedded catalog + update cache + custom_models.json overlay).
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// Target decode throughput. Sets the bandwidth floor via
	// tok/s ≈ bandwidth × efficiency / weights.
	// +kubebuilder:default=20
	// +optional
	MinTps *float64 `json:"minTps,omitempty"`

	// Quantization override (default = catalog quant).
	// +optional
	Quant string `json:"quant,omitempty"`

	// Bandwidth efficiency assumption (percent).
	// +kubebuilder:default=55
	// +optional
	EfficiencyPct *int32 `json:"efficiencyPct,omitempty"`

	// DeviceClass the emitted request targets. gpu.llmfit.ai refuses the CPU
	// fallback; llmfit.ai allows any kind.
	// +kubebuilder:default="llmfit.ai"
	// +optional
	DeviceClassName string `json:"deviceClassName,omitempty"`

	// TargetDriver selects which DRA driver's devices the emitted fit CEL
	// reads. Defaults by DeviceClass name: the NVIDIA classes
	// (gpu.nvidia.com / mig.nvidia.com) imply gpu.nvidia.com; everything else
	// implies llmfit.ai. Set explicitly only with a custom DeviceClass that
	// selects NVIDIA-driver devices. On the NVIDIA target, per-MIG-slice
	// bandwidth is llmfit's DERIVED model (board peak × memory-slice
	// fraction) — NVIDIA publishes no per-profile bandwidth.
	// +kubebuilder:validation:Enum=llmfit.ai;gpu.nvidia.com
	// +optional
	TargetDriver string `json:"targetDriver,omitempty"`

	// Compute floor in effective dense FP16 TFLOPS, for prefill/TTFT-bound
	// stages (prefill is compute-bound where decode is bandwidth-bound).
	// Opt-in: when set, the fit CEL additionally requires
	// device.attributes['llmfit.ai'].computeTFLOPS >= this value, so devices
	// publishing no compute number do not match — on any class, including CPU.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinComputeTFLOPS *int64 `json:"minComputeTFLOPS,omitempty"`

	// Extra CEL expressions ANDed onto the generated selector (DRA ANDs all
	// selectors). Never replaces the physics.
	// +optional
	ExtraSelectors []string `json:"extraSelectors,omitempty"`
}

// ResolvedPhysics captures the fit-inequality inputs the resolver derived
// from spec.model — the literal numbers inlined into the template's CEL.
type ResolvedPhysics struct {
	// +optional
	MemoryGi int64 `json:"memoryGi,omitempty"`
	// +optional
	MinBandwidthGBs int64 `json:"minBandwidthGBs,omitempty"`
	// +optional
	Quant string `json:"quant,omitempty"`
	// +optional
	WeightsGb string `json:"weightsGb,omitempty"`
	// +optional
	ResolverVersion string `json:"resolverVersion,omitempty"`
}

// TemplateRef names the same-named ResourceClaimTemplate the controller
// reconciles for this claim.
type TemplateRef struct {
	// +optional
	Name string `json:"name,omitempty"`
}

// Candidates counts published devices against the resolved bounds:
// physics-satisfiable (devices/nodes) versus available-right-now (available).
type Candidates struct {
	// +optional
	Devices int64 `json:"devices,omitempty"`
	// +optional
	Nodes int64 `json:"nodes,omitempty"`
	// +optional
	Available int64 `json:"available,omitempty"`
}

// ModelClaimStatus reports resolution and satisfiability.
type ModelClaimStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Resolved *ResolvedPhysics `json:"resolved,omitempty"`

	// +optional
	TemplateRef *TemplateRef `json:"templateRef,omitempty"`

	// +optional
	Candidates *Candidates `json:"candidates,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelClaim is "run <model>" as a Kubernetes object
// (docs/design/modelclaim.md). The controller resolves spec.model against
// llmfit's database and reconciles a SAME-NAMED ResourceClaimTemplate; pods
// reference it with plain `resourceClaimTemplateName: <name>`. Structural
// validation lives in the CRD schema (+ CEL rules) — no admission webhooks.
// Semantic errors (model not in the database) surface asynchronously as the
// Resolved condition.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mclaim
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="MinTPS",type=number,JSONPath=`.spec.minTps`
// +kubebuilder:printcolumn:name="Resolved",type=string,JSONPath=`.status.conditions[?(@.type=="Resolved")].status`
// +kubebuilder:printcolumn:name="Satisfiable",type=string,JSONPath=`.status.conditions[?(@.type=="Satisfiable")].status`
// +kubebuilder:printcolumn:name="Devices",type=integer,JSONPath=`.status.candidates.devices`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.candidates.available`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ModelClaimSpec `json:"spec"`
	// +optional
	Status ModelClaimStatus `json:"status,omitempty"`
}

// ModelClaimList contains a list of ModelClaim.
// +kubebuilder:object:root=true
type ModelClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelClaim `json:"items"`
}
