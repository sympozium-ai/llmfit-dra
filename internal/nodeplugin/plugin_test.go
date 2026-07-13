package nodeplugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/sympozium-ai/llmfit-dra/internal/publisher"
	"github.com/sympozium-ai/llmfit-dra/pkg/probe"
)

func testInventory() *Inventory {
	inv := NewInventory()
	inv.Set([]probe.Device{
		{Kind: probe.KindGPU, Index: 0, Driver: "amdgpu", DevNode: "/dev/dri/card1", RenderNode: "/dev/dri/renderD128"},
		{Kind: probe.KindNPU, Index: 0, Driver: "intel_vpu", DevNode: "/dev/accel/accel0"},
		{Kind: probe.KindCPU, Index: 0},
	})
	return inv
}

func claimFor(uid, deviceName string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", UID: types.UID(uid)},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Request: "accel",
						Driver:  publisher.DriverName,
						Pool:    "node1",
						Device:  deviceName,
					}},
				},
			},
		},
	}
}

func prepareOne(t *testing.T, p *Plugin, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	t.Helper()
	results, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims: %v", err)
	}
	res, ok := results[claim.UID]
	if !ok {
		t.Fatalf("no result for claim %s", claim.UID)
	}
	return res
}

func readSpec(t *testing.T, dir, uid string) cdiSpec {
	t.Helper()
	data, err := os.ReadFile(specPath(dir, uid))
	if err != nil {
		t.Fatalf("reading CDI spec: %v", err)
	}
	var spec cdiSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("CDI spec not valid JSON: %v", err)
	}
	return spec
}

func TestPrepareGPUWritesCDISpec(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	claim := claimFor("uid-1", "gpu0")

	res := prepareOne(t, p, claim)
	if res.Err != nil {
		t.Fatalf("prepare failed: %v", res.Err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("expected 1 prepared device, got %+v", res.Devices)
	}
	dev := res.Devices[0]
	if dev.DeviceName != "gpu0" || dev.PoolName != "node1" || !reflect.DeepEqual(dev.Requests, []string{"accel"}) {
		t.Errorf("prepared device mismatch: %+v", dev)
	}
	wantID := "llmfit.ai/device=uid-1-gpu0"
	if !reflect.DeepEqual(dev.CDIDeviceIDs, []string{wantID}) {
		t.Errorf("CDI IDs = %v, want [%s]", dev.CDIDeviceIDs, wantID)
	}

	spec := readSpec(t, dir, "uid-1")
	if spec.Kind != "llmfit.ai/device" || spec.CDIVersion != "0.6.0" {
		t.Errorf("spec header = %s/%s", spec.Kind, spec.CDIVersion)
	}
	if len(spec.Devices) != 1 || spec.Devices[0].Name != "uid-1-gpu0" {
		t.Fatalf("spec devices = %+v", spec.Devices)
	}
	edits := spec.Devices[0].ContainerEdits
	wantEnv := []string{"LLMFIT_DEVICE=gpu0", "LLMFIT_DEVICE_KIND=gpu", "LLMFIT_DEVICE_GPU0=/dev/dri/renderD128", "LLMFIT_RENDER_NODE=/dev/dri/renderD128"}
	if !reflect.DeepEqual(edits.Env, wantEnv) {
		t.Errorf("env = %v, want %v", edits.Env, wantEnv)
	}
	// amdgpu: render node + card node + node-global /dev/kfd.
	wantNodes := []cdiDeviceNode{{Path: "/dev/dri/renderD128"}, {Path: "/dev/dri/card1"}, {Path: "/dev/kfd"}}
	if !reflect.DeepEqual(edits.DeviceNodes, wantNodes) {
		t.Errorf("device nodes = %v, want %v", edits.DeviceNodes, wantNodes)
	}
}

func TestPrepareCPUIsEnvOnly(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	res := prepareOne(t, p, claimFor("uid-2", "cpu0"))
	if res.Err != nil {
		t.Fatalf("prepare failed: %v", res.Err)
	}
	edits := readSpec(t, dir, "uid-2").Devices[0].ContainerEdits
	if len(edits.DeviceNodes) != 0 {
		t.Errorf("cpu0 must not inject device nodes, got %v", edits.DeviceNodes)
	}
	wantEnv := []string{"LLMFIT_DEVICE=cpu0", "LLMFIT_DEVICE_KIND=cpu", "LLMFIT_DEVICE_CPU0=cpu"}
	if !reflect.DeepEqual(edits.Env, wantEnv) {
		t.Errorf("env = %v, want %v", edits.Env, wantEnv)
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	claim := claimFor("uid-3", "npu0")
	first := prepareOne(t, p, claim)
	second := prepareOne(t, p, claim)
	if first.Err != nil || second.Err != nil {
		t.Fatalf("prepare errs: %v / %v", first.Err, second.Err)
	}
	if !reflect.DeepEqual(first.Devices, second.Devices) {
		t.Errorf("prepare not idempotent: %+v vs %+v", first.Devices, second.Devices)
	}
}

func TestPrepareUnknownDeviceFailsThatClaimOnly(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	bad := claimFor("uid-bad", "gpu9")
	good := claimFor("uid-good", "gpu0")
	results, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{bad, good})
	if err != nil {
		t.Fatalf("batch must not fail: %v", err)
	}
	if results[bad.UID].Err == nil {
		t.Error("expected error for device missing from inventory")
	}
	if results[good.UID].Err != nil {
		t.Errorf("good claim must survive the bad one: %v", results[good.UID].Err)
	}
	if _, err := os.Stat(specPath(dir, "uid-bad")); !os.IsNotExist(err) {
		t.Error("no CDI spec should exist for the failed claim")
	}
}

func TestPrepareSkipsForeignDriverResults(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	claim := claimFor("uid-4", "gpu0")
	claim.Status.Allocation.Devices.Results = append(claim.Status.Allocation.Devices.Results,
		resourceapi.DeviceRequestAllocationResult{Request: "vendor", Driver: "gpu.nvidia.com", Pool: "node1", Device: "gpu-0"})
	res := prepareOne(t, p, claim)
	if res.Err != nil {
		t.Fatalf("prepare failed: %v", res.Err)
	}
	if len(res.Devices) != 1 || res.Devices[0].DeviceName != "gpu0" {
		t.Errorf("expected only our device prepared, got %+v", res.Devices)
	}
}

func TestUnprepareRemovesSpecAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := &Plugin{inv: testInventory(), cdiDir: dir}
	claim := claimFor("uid-5", "gpu0")
	if res := prepareOne(t, p, claim); res.Err != nil {
		t.Fatal(res.Err)
	}
	ref := kubeletplugin.NamespacedObject{
		UID:            claim.UID,
		NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name},
	}
	for i := 0; i < 2; i++ { // second pass: already gone must still succeed
		results, err := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{ref})
		if err != nil {
			t.Fatalf("UnprepareResourceClaims: %v", err)
		}
		if results[claim.UID] != nil {
			t.Fatalf("unprepare pass %d: %v", i+1, results[claim.UID])
		}
	}
	if _, err := os.Stat(specPath(dir, string(claim.UID))); !os.IsNotExist(err) {
		t.Error("CDI spec still present after unprepare")
	}
	// The spec dir must not accumulate temp files either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("leftover file in CDI dir: %s", filepath.Join(dir, e.Name()))
	}
}

// TestPrepareNICInjectsVerbsAndRDMACM: a NIC's edits are its uverbs node
// plus the node-global /dev/infiniband/rdma_cm (librdmacm can't connect
// without it), and LLMFIT_NETDEV names the paired RoCE interface.
func TestPrepareNICInjectsVerbsAndRDMACM(t *testing.T) {
	dir := t.TempDir()
	inv := NewInventory()
	inv.Set([]probe.Device{{
		Kind: probe.KindNIC, Index: 0, Driver: "mlx5_core",
		PCIAddr: "0000:41:00.0", DevNode: "/dev/infiniband/uverbs0",
		NetDev: "eth401", IBPortActive: true,
	}})
	p := &Plugin{inv: inv, cdiDir: dir}

	res := prepareOne(t, p, claimFor("uid-nic", "nic-0000-41-00-0"))
	if res.Err != nil {
		t.Fatalf("prepare failed: %v", res.Err)
	}
	edits := readSpec(t, dir, "uid-nic").Devices[0].ContainerEdits
	wantNodes := []cdiDeviceNode{{Path: "/dev/infiniband/uverbs0"}, {Path: "/dev/infiniband/rdma_cm"}}
	if !reflect.DeepEqual(edits.DeviceNodes, wantNodes) {
		t.Errorf("device nodes = %v, want %v", edits.DeviceNodes, wantNodes)
	}
	wantEnv := []string{
		"LLMFIT_DEVICE=nic-0000-41-00-0",
		"LLMFIT_DEVICE_KIND=nic",
		"LLMFIT_DEVICE_NIC_0000_41_00_0=/dev/infiniband/uverbs0",
		"LLMFIT_NETDEV=eth401",
	}
	if !reflect.DeepEqual(edits.Env, wantEnv) {
		t.Errorf("env = %v, want %v", edits.Env, wantEnv)
	}
}
