// Package nodeplugin is the kubelet-facing half of the driver (Phase 2):
// it serves NodePrepareResources/NodeUnprepareResources via the upstream
// kubeletplugin helper and translates allocated devices into CDI container
// edits. The publisher makes devices schedulable; this makes them usable.
//
// Preparation is deliberately thin: device nodes plus LLMFIT_* env. The
// device was chosen by CEL at claim time — nothing model-specific happens
// here, per the "physics inputs, not verdicts" tenet. cpu0 prepares to an
// env-only edit (no device nodes), which keeps the full claim→Running path
// exercisable on clusters without accelerators.
package nodeplugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/sympozium-ai/llmfit-dra/internal/probe"
	"github.com/sympozium-ai/llmfit-dra/internal/publisher"
)

// Inventory is the probe snapshot shared between the re-probe loop (writer)
// and Prepare calls (readers). Device names (gpu0…) are the join key with
// allocation results.
type Inventory struct {
	mu      sync.RWMutex
	devices map[string]probe.Device
}

func NewInventory() *Inventory {
	return &Inventory{devices: map[string]probe.Device{}}
}

func (inv *Inventory) Set(devices []probe.Device) {
	m := make(map[string]probe.Device, len(devices))
	for _, d := range devices {
		m[d.Name()] = d
	}
	inv.mu.Lock()
	inv.devices = m
	inv.mu.Unlock()
}

func (inv *Inventory) lookup(name string) (probe.Device, bool) {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	d, ok := inv.devices[name]
	return d, ok
}

// Plugin implements kubeletplugin.DRAPlugin.
type Plugin struct {
	inv    *Inventory
	cdiDir string
}

// Start registers the plugin with the kubelet. The helper owns the sockets
// (registrar under /var/lib/kubelet/plugins_registry, service under
// /var/lib/kubelet/plugins/llmfit.ai) — the DaemonSet must mount both dirs
// plus cdiDir from the host.
func Start(ctx context.Context, client kubernetes.Interface, nodeName string, inv *Inventory, cdiDir string) (*kubeletplugin.Helper, error) {
	// The helper listens in this directory but does not create it.
	if err := os.MkdirAll(filepath.Join(kubeletplugin.KubeletPluginsDir, publisher.DriverName), 0o750); err != nil {
		return nil, fmt.Errorf("creating plugin socket dir: %w", err)
	}
	return kubeletplugin.Start(ctx, &Plugin{inv: inv, cdiDir: cdiDir},
		kubeletplugin.DriverName(publisher.DriverName),
		kubeletplugin.KubeClient(client),
		kubeletplugin.NodeName(nodeName),
	)
}

// PrepareResourceClaims writes one CDI spec per claim and maps each
// allocated device to its CDI ID. Per-claim failures are reported in the
// result map so one bad claim doesn't fail the batch.
func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		results[claim.UID] = p.prepareClaim(ctx, claim)
	}
	return results, nil
}

func (p *Plugin) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name)}
	}
	var prepared []kubeletplugin.Device
	var cdiDevices []cdiDevice
	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != publisher.DriverName {
			continue // another driver's result in a mixed claim (Phase 3 territory)
		}
		dev, ok := p.inv.lookup(alloc.Device)
		if !ok {
			// Allocation references a device the current probe doesn't see —
			// hardware went away between scheduling and prepare.
			return kubeletplugin.PrepareResult{Err: fmt.Errorf("allocated device %q not in current inventory", alloc.Device)}
		}
		name := string(claim.UID) + "-" + alloc.Device
		cdiDevices = append(cdiDevices, cdiDevice{Name: name, ContainerEdits: editsFor(dev)})
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: []string{qualifiedName(name)},
		})
	}
	if len(prepared) == 0 {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no %s allocation results", claim.Namespace, claim.Name, publisher.DriverName)}
	}
	if err := writeSpec(p.cdiDir, string(claim.UID), cdiDevices); err != nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("writing CDI spec: %w", err)}
	}
	klog.FromContext(ctx).V(2).Info("prepared claim", "claim", klog.KObj(claim), "devices", len(prepared))
	return kubeletplugin.PrepareResult{Devices: prepared}
}

// editsFor maps a probed device to its container edits: every /dev node the
// device needs, plus LLMFIT_* env identifying what was granted. amdgpu
// compute additionally requires the node-global /dev/kfd (KFD is how ROCm
// reaches the GPU; the render node alone only covers Vulkan).
func editsFor(d probe.Device) containerEdits {
	edits := containerEdits{
		Env: []string{
			"LLMFIT_DEVICE=" + d.Name(),
			"LLMFIT_DEVICE_KIND=" + string(d.Kind),
		},
	}
	if d.RenderNode != "" {
		edits.Env = append(edits.Env, "LLMFIT_RENDER_NODE="+d.RenderNode)
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: d.RenderNode})
	}
	if d.DevNode != "" {
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: d.DevNode})
	}
	if d.Driver == "amdgpu" {
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: "/dev/kfd"})
	}
	return edits
}

// HandleError receives background failures from the helper's gRPC servers.
// Everything the plugin serves is retried by the kubelet, so log loudly and
// keep running; the DaemonSet restart path covers truly wedged sockets.
func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	klog.FromContext(ctx).Error(err, msg)
}

// UnprepareResourceClaims removes each claim's CDI spec. The kubelet may call
// this for claims a previous driver instance prepared, so missing files are
// success, and errors are reported per claim.
func (p *Plugin) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		results[claim.UID] = removeSpec(p.cdiDir, string(claim.UID))
	}
	return results, nil
}
