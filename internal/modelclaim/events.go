package modelclaim

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/sympozium-ai/llmfit-dra/api/v1alpha1"
)

// eventFor builds a core Event attached to the ModelClaim, so
// `kubectl describe modelclaim` explains resolution outcomes. APIVersion and
// Kind come from package constants — typed objects legitimately carry an
// empty TypeMeta.
func eventFor(mc *apiv1alpha1.ModelClaim, kind, reason, message string, now metav1.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: mc.Name + ".",
			Namespace:    mc.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       apiv1alpha1.ModelClaimKind,
			Namespace:  mc.Namespace,
			Name:       mc.Name,
			UID:        mc.UID,
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
