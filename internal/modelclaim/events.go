package modelclaim

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// eventFor builds a core Event attached to the ModelClaim, so
// `kubectl describe modelclaim` explains resolution outcomes.
func eventFor(mc *unstructured.Unstructured, kind, reason, message string, now metav1.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: mc.GetName() + ".",
			Namespace:    mc.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: mc.GetAPIVersion(),
			Kind:       mc.GetKind(),
			Namespace:  mc.GetNamespace(),
			Name:       mc.GetName(),
			UID:        mc.GetUID(),
		},
		Type:           kind,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: "llmfit-dra-modelclaim"},
	}
}
