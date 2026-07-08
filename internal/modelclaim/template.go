package modelclaim

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	apiv1alpha1 "github.com/sympozium-ai/llmfit-dra/api/v1alpha1"
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
//
// The CPU class is the one deliberate divergence: CPU devices publish no
// memoryBandwidthGBs (llmfit has no per-platform DRAM-bandwidth data), so
// keeping the bandwidth clause would make cpu.llmfit.ai structurally
// unsatisfiable for every ModelClaim. Naming the CPU class is an explicit
// "I accept CPU speed" — the memory fit still holds; the tok/s floor is
// waived.
//
// minComputeTFLOPS is the OPT-IN compute floor (prefill/TTFT physics). It is
// claim intent rather than resolved physics — deliberately not part of
// Bounds, which is shared through the resolve cache — and unlike bandwidth
// it is NOT waived for the CPU class: setting an explicit floor on a class
// whose devices publish no compute number is a contradiction the Satisfiable
// condition should say out loud, not paper over.
func FitCEL(b *Bounds, deviceClassName string, minComputeTFLOPS int64) string {
	d := DriverDomain
	var expr string
	if deviceClassName == "cpu."+d {
		expr = fmt.Sprintf(
			"'memory' in device.capacity['%[1]s'] && "+
				"device.capacity['%[1]s'].memory.compareTo(quantity('%[2]dGi')) >= 0 && "+
				"'healthy' in device.attributes['%[1]s'] && "+
				"device.attributes['%[1]s'].healthy",
			d, b.MemoryGi)
	} else {
		expr = fmt.Sprintf(
			"'memory' in device.capacity['%[1]s'] && "+
				"device.capacity['%[1]s'].memory.compareTo(quantity('%[2]dGi')) >= 0 && "+
				"'memoryBandwidthGBs' in device.attributes['%[1]s'] && "+
				"device.attributes['%[1]s'].memoryBandwidthGBs >= %[3]d && "+
				"'healthy' in device.attributes['%[1]s'] && "+
				"device.attributes['%[1]s'].healthy",
			d, b.MemoryGi, b.MinBandwidthGBs)
	}
	if minComputeTFLOPS > 0 {
		expr += fmt.Sprintf(" && "+
			"'computeTFLOPS' in device.attributes['%[1]s'] && "+
			"device.attributes['%[1]s'].computeTFLOPS >= %[2]d",
			d, minComputeTFLOPS)
	}
	return expr
}

// BuildTemplate renders the desired ResourceClaimTemplate for a ModelClaim:
// same name/namespace, owned by the claim (GC), one device request whose
// selectors are the generated fit CEL ANDed with any extraSelectors (DRA
// ANDs all selectors on a request).
func BuildTemplate(mc *apiv1alpha1.ModelClaim, b *Bounds, deviceClassName string, extraSelectors []string) *resourceapi.ResourceClaimTemplate {
	selectors := []resourceapi.DeviceSelector{{
		CEL: &resourceapi.CELDeviceSelector{Expression: FitCEL(b, deviceClassName, computeFloor(mc))},
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
			Name:      mc.Name,
			Namespace: mc.Namespace,
			Labels:    map[string]string{ManagedByLabel: labelValue(mc.Name)},
			Annotations: map[string]string{
				"llmfit.ai/model":            b.Model,
				"llmfit.ai/quant":            b.Quant,
				"llmfit.ai/resolver-version": b.ResolverVersion,
			},
			// APIVersion/Kind from package constants, not mc.TypeMeta — typed
			// objects legitimately carry an empty TypeMeta.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         apiv1alpha1.GroupVersion.String(),
				Kind:               apiv1alpha1.ModelClaimKind,
				Name:               mc.Name,
				UID:                mc.UID,
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

// computeFloor reads the opt-in compute floor from the claim spec (0 = unset).
func computeFloor(mc *apiv1alpha1.ModelClaim) int64 {
	if mc.Spec.MinComputeTFLOPS != nil {
		return *mc.Spec.MinComputeTFLOPS
	}
	return 0
}

// labelValue makes s a valid label value: CR names may run to 253 chars but
// label values cap at 63 — a too-long name must not make the template
// permanently uncreatable. Ownership does not ride on this label (that's the
// controller ownerRef); it is informational.
func labelValue(s string) string {
	if len(s) <= 63 {
		return s
	}
	s = s[:63]
	return strings.TrimRight(s, "-_.")
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
