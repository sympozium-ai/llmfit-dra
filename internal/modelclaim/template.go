package modelclaim

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

const (
	// DriverDomain scopes the attributes/capacity the generated CEL reads.
	DriverDomain = "llmfit.ai"
	// ManagedByLabel marks templates this controller owns.
	ManagedByLabel = "llmfit.ai/modelclaim"
)

// FitCEL renders the guarded fit inequality for the resolved bounds — the
// same expression `llmfit claim` emits (llmfit-core claim.rs), kept in
// lockstep by the golden test. Every optional lookup is membership-guarded:
// a missing attribute must mean "no match", not a CEL runtime error that
// disqualifies the node (llmfit#669).
func FitCEL(b *Bounds) string {
	d := DriverDomain
	return fmt.Sprintf(
		"'memory' in device.capacity['%[1]s'] && "+
			"device.capacity['%[1]s'].memory.compareTo(quantity('%[2]dGi')) >= 0 && "+
			"'memoryBandwidthGBs' in device.attributes['%[1]s'] && "+
			"device.attributes['%[1]s'].memoryBandwidthGBs >= %[3]d && "+
			"'healthy' in device.attributes['%[1]s'] && "+
			"device.attributes['%[1]s'].healthy",
		d, b.MemoryGi, b.MinBandwidthGBs)
}

// BuildTemplate renders the desired ResourceClaimTemplate for a ModelClaim:
// same name/namespace, owned by the claim (GC), one device request whose
// selectors are the generated fit CEL ANDed with any extraSelectors (DRA
// ANDs all selectors on a request).
func BuildTemplate(mc *unstructured.Unstructured, b *Bounds, deviceClassName string, extraSelectors []string) *resourceapi.ResourceClaimTemplate {
	selectors := []resourceapi.DeviceSelector{{
		CEL: &resourceapi.CELDeviceSelector{Expression: FitCEL(b)},
	}}
	for _, cel := range extraSelectors {
		if s := strings.TrimSpace(cel); s != "" {
			selectors = append(selectors, resourceapi.DeviceSelector{
				CEL: &resourceapi.CELDeviceSelector{Expression: s},
			})
		}
	}
	return &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mc.GetName(),
			Namespace: mc.GetNamespace(),
			Labels:    map[string]string{ManagedByLabel: mc.GetName()},
			Annotations: map[string]string{
				"llmfit.ai/model":            b.Model,
				"llmfit.ai/quant":            b.Quant,
				"llmfit.ai/resolver-version": b.ResolverVersion,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         mc.GetAPIVersion(),
				Kind:               mc.GetKind(),
				Name:               mc.GetName(),
				UID:                mc.GetUID(),
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{
					Requests: []resourceapi.DeviceRequest{{
						Name: "model",
						Exactly: &resourceapi.ExactDeviceRequest{
							DeviceClassName: deviceClassName,
							Selectors:       selectors,
						},
					}},
				},
			},
		},
	}
}

// TemplateNeedsUpdate reports whether the live template's spec or managed
// metadata drifted from desired.
func TemplateNeedsUpdate(live, desired *resourceapi.ResourceClaimTemplate) bool {
	if len(live.Spec.Spec.Devices.Requests) != len(desired.Spec.Spec.Devices.Requests) {
		return true
	}
	for i := range desired.Spec.Spec.Devices.Requests {
		l, d := live.Spec.Spec.Devices.Requests[i], desired.Spec.Spec.Devices.Requests[i]
		if l.Name != d.Name {
			return true
		}
		if (l.Exactly == nil) != (d.Exactly == nil) {
			return true
		}
		if l.Exactly == nil {
			continue
		}
		if l.Exactly.DeviceClassName != d.Exactly.DeviceClassName {
			return true
		}
		if len(l.Exactly.Selectors) != len(d.Exactly.Selectors) {
			return true
		}
		for j := range d.Exactly.Selectors {
			ls, ds := l.Exactly.Selectors[j], d.Exactly.Selectors[j]
			if (ls.CEL == nil) != (ds.CEL == nil) {
				return true
			}
			if ls.CEL != nil && ls.CEL.Expression != ds.CEL.Expression {
				return true
			}
		}
	}
	for k, v := range desired.Annotations {
		if live.Annotations[k] != v {
			return true
		}
	}
	return false
}
