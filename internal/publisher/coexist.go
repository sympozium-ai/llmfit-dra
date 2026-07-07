package publisher

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultVendorDrivers are DRA drivers known to own GPU allocation on nodes
// where they run. When one publishes ResourceSlices for our node, llmfit's
// GPUs are demoted to fitness-only (vendorManaged attribute; the shipped
// DeviceClasses exclude them) so the same silicon is never allocatable
// through two drivers at once. Unrelated DRA drivers (NICs, FPGAs) must NOT
// trigger the demotion — hence a known list, not "any other driver".
// (gpu.amd.com is AMD's DRA driver: on an AMD-heavy fleet its absence here
// was the most likely real double-booking, since amdgpu GPUs are otherwise
// fully preparable through our default classes.)
const DefaultVendorDrivers = "gpu.nvidia.com,gpu.amd.com,gpu.intel.com,neuron.amazonaws.com"

// ParseVendorDrivers splits the --vendor-drivers flag; empty disables
// coexistence demotion entirely.
func ParseVendorDrivers(flag string) map[string]bool {
	out := map[string]bool{}
	for _, d := range strings.Split(flag, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out[d] = true
		}
	}
	return out
}

// VendorGPUsPresent reports whether any known vendor GPU driver publishes
// ResourceSlices for this node, plus the sorted set of OTHER foreign DRA
// drivers seen (not ours, not in the known list) — callers surface those so
// an unrecognized vendor driver is a visible gap in the coexistence list
// instead of a silent double-booking. One field-selected list per probe
// cycle — the same selector the scheduler and kubelet use, so it stays
// O(this node's slices) regardless of cluster size.
func VendorGPUsPresent(ctx context.Context, client kubernetes.Interface, nodeName string, vendors map[string]bool) (bool, []string, error) {
	if len(vendors) == 0 {
		return false, nil, nil
	}
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return false, nil, err
	}
	present := false
	unknown := map[string]bool{}
	for _, s := range slices.Items {
		switch {
		case vendors[s.Spec.Driver]:
			present = true
		case s.Spec.Driver != DriverName:
			unknown[s.Spec.Driver] = true
		}
	}
	others := make([]string, 0, len(unknown))
	for d := range unknown {
		others = append(others, d)
	}
	sort.Strings(others)
	return present, others, nil
}
