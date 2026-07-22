package modelclaim

import (
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// nvSliceDevice builds a device the way gpu.nvidia.com publishes them.
func nvSliceDevice(name, devType, product, profile, memory string) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":        {StringValue: ptr.To(devType)},
		"productName": {StringValue: ptr.To(product)},
	}
	if profile != "" {
		attrs["profile"] = resourceapi.DeviceAttribute{StringValue: ptr.To(profile)}
	}
	return resourceapi.Device{
		Name:       name,
		Attributes: attrs,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {Value: resource.MustParse(memory)},
		},
	}
}

func nvSlice(node, pool string, gen int64, devs ...resourceapi.Device) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   NvidiaDriverDomain,
			NodeName: ptr.To(node),
			Pool:     resourceapi.ResourcePool{Name: pool, Generation: gen},
			Devices:  devs,
		},
	}
}

func TestEvaluateNvidiaSlicesMIG(t *testing.T) {
	boards := testBoards(t)
	slices := []*resourceapi.ResourceSlice{
		nvSlice("node-a", "node-a-gpu0", 1,
			nvSliceDevice("gpu-0-mig-1g-10gb-0", "mig", "NVIDIA A100-SXM4-80GB", "1g.10gb", "9856Mi"),
			nvSliceDevice("gpu-0-mig-2g-20gb-0", "mig", "NVIDIA A100-SXM4-80GB", "2g.20gb", "19968Mi"),
			nvSliceDevice("gpu-0-mig-3g-40gb-0", "mig", "NVIDIA A100-SXM4-80GB", "3g.40gb", "40192Mi"),
			nvSliceDevice("gpu-0-mig-3g-40gb-1", "mig", "NVIDIA A100-SXM4-80GB", "3g.40gb", "40192Mi"),
		),
	}

	c := EvaluateNvidiaSlices(slices, nil, nvidiaBounds(), "mig.nvidia.com", 0, boards)
	if c.Devices != 2 || c.Nodes != 1 || c.Available != 2 {
		t.Errorf("want 2 devices / 1 node / 2 available, got %+v", c)
	}

	// One 3g slice held by an allocated claim: available drops, holder named.
	held := map[string]string{"node-a-gpu0/gpu-0-mig-3g-40gb-0": "team-a/pod-claim-1"}
	c = EvaluateNvidiaSlices(slices, held, nvidiaBounds(), "mig.nvidia.com", 0, boards)
	if c.Devices != 2 || c.Available != 1 || c.HeldBy != "team-a/pod-claim-1" {
		t.Errorf("held accounting wrong: %+v", c)
	}
}

func TestEvaluateNvidiaSlicesShortfallDerived(t *testing.T) {
	boards := testBoards(t)
	slices := []*resourceapi.ResourceSlice{
		nvSlice("node-a", "node-a-gpu0", 1,
			nvSliceDevice("gpu-0-mig-2g-20gb-0", "mig", "NVIDIA A100-SXM4-80GB", "2g.20gb", "19968Mi"),
		),
	}
	c := EvaluateNvidiaSlices(slices, nil, nvidiaBounds(), "mig.nvidia.com", 0, boards)
	if c.Devices != 0 {
		t.Fatalf("2g.20gb must not satisfy 644 GB/s, got %+v", c)
	}
	for _, want := range []string{"2g.20gb", "derived bandwidth", "644", "NVIDIA A100-SXM4-80GB", "node-a"} {
		if !strings.Contains(c.Shortfall, want) {
			t.Errorf("shortfall missing %q: %s", want, c.Shortfall)
		}
	}
}

func TestEvaluateNvidiaSlicesUnknownBoard(t *testing.T) {
	boards := testBoards(t)
	slices := []*resourceapi.ResourceSlice{
		nvSlice("node-a", "node-a-gpu0", 1,
			nvSliceDevice("gpu-0", "gpu", "NVIDIA B300", "", "192Gi"),
		),
	}
	c := EvaluateNvidiaSlices(slices, nil, nvidiaBounds(), "gpu.nvidia.com", 0, boards)
	if c.Devices != 0 {
		t.Fatalf("unknown board must fail closed, got %+v", c)
	}
	for _, want := range []string{`"NVIDIA B300"`, "board table", "fail closed", boards.Version()} {
		if !strings.Contains(c.Shortfall, want) {
			t.Errorf("shortfall missing %q: %s", want, c.Shortfall)
		}
	}
}

func TestEvaluateNvidiaSlicesPoolGeneration(t *testing.T) {
	boards := testBoards(t)
	// Same pool at generations 1 and 2 — only the newer counts.
	slices := []*resourceapi.ResourceSlice{
		nvSlice("node-a", "node-a-gpu0", 1,
			nvSliceDevice("gpu-0-mig-3g-40gb-0", "mig", "NVIDIA A100-SXM4-80GB", "3g.40gb", "40192Mi"),
		),
		nvSlice("node-a", "node-a-gpu0", 2,
			nvSliceDevice("gpu-0-mig-3g-40gb-0", "mig", "NVIDIA A100-SXM4-80GB", "3g.40gb", "40192Mi"),
		),
	}
	c := EvaluateNvidiaSlices(slices, nil, nvidiaBounds(), "mig.nvidia.com", 0, boards)
	if c.Devices != 1 {
		t.Errorf("superseded pool generation double-counted: %+v", c)
	}
}

func TestEvaluateNvidiaSlicesIgnoresOtherDrivers(t *testing.T) {
	boards := testBoards(t)
	llmfitSlice := &resourceapi.ResourceSlice{
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   DriverDomain,
			NodeName: ptr.To("node-b"),
			Pool:     resourceapi.ResourcePool{Name: "node-b", Generation: 1},
			Devices: []resourceapi.Device{
				nvSliceDevice("gpu-x", "gpu", "NVIDIA A100-SXM4-80GB", "", "80Gi"),
			},
		},
	}
	c := EvaluateNvidiaSlices([]*resourceapi.ResourceSlice{llmfitSlice}, nil, nvidiaBounds(), "gpu.nvidia.com", 0, boards)
	if c.Devices != 0 || !strings.Contains(c.Shortfall, "no gpu.nvidia.com devices published") {
		t.Errorf("llmfit.ai slices must be invisible here: %+v", c)
	}
}

func TestEvaluateNvidiaSlicesGPUBranch(t *testing.T) {
	boards := testBoards(t)
	slices := []*resourceapi.ResourceSlice{
		nvSlice("node-a", "node-a-gpu0", 1,
			nvSliceDevice("gpu-0", "gpu", "NVIDIA A100-SXM4-80GB", "", "80Gi"),
			// Board peak 600 GB/s < 644: whole device rejected at board level.
			nvSliceDevice("gpu-1", "gpu", "NVIDIA A10", "", "24Gi"),
			// MIG device invisible to the gpu-only class.
			nvSliceDevice("gpu-2-mig", "mig", "NVIDIA A100-SXM4-80GB", "3g.40gb", "40192Mi"),
		),
	}
	c := EvaluateNvidiaSlices(slices, nil, nvidiaBounds(), "gpu.nvidia.com", 0, boards)
	if c.Devices != 1 || c.Available != 1 {
		t.Errorf("want exactly the full A100 admitted, got %+v", c)
	}
}
