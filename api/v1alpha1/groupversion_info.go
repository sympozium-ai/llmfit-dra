// Package v1alpha1 contains the llmfit.ai v1alpha1 API types.
//
// ModelClaim is the public contract of llmfit-dra: "run <model>" as a
// Kubernetes object (docs/design/modelclaim.md). This package is a separate
// Go module so orchestrators can depend on the types without importing the
// driver.
//
// +kubebuilder:object:generate=true
// +groupName=llmfit.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group served by the llmfit-dra ModelClaim controller.
const GroupName = "llmfit.ai"

// ModelClaimKind is the CRD kind, for ownerRefs and event refs built without
// relying on a populated TypeMeta.
const ModelClaimKind = "ModelClaim"

var (
	// GroupVersion identifies the group/version of the types in this package.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder collects the functions that register this group-version.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers the ModelClaim types with the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&ModelClaim{},
		&ModelClaimList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
