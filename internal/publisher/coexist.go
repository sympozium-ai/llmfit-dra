package publisher

import (
	"context"
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
const DefaultVendorDrivers = "gpu.nvidia.com,gpu.intel.com,neuron.amazonaws.com"

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
// ResourceSlices for this node. One field-selected list per probe cycle —
// the same selector the scheduler and kubelet use, so it stays O(this
// node's slices) regardless of cluster size.
func VendorGPUsPresent(ctx context.Context, client kubernetes.Interface, nodeName string, vendors map[string]bool) (bool, error) {
	if len(vendors) == 0 {
		return false, nil
	}
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return false, err
	}
	for _, s := range slices.Items {
		if vendors[s.Spec.Driver] {
			return true, nil
		}
	}
	return false, nil
}
