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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/sympozium-ai/llmfit-dra/internal/observe"
	"github.com/sympozium-ai/llmfit-dra/internal/publisher"
	"github.com/sympozium-ai/llmfit-dra/pkg/probe"
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

// driverCount reports how many devices are bound to the given kernel driver
// — Prepare uses it to decide whether ROCm visibility isolation is needed.
func (inv *Inventory) driverCount(driver string) int {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	n := 0
	for _, d := range inv.devices {
		if d.Driver == driver {
			n++
		}
	}
	return n
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
//
// A non-empty podUID (from the downward API) enables the helper's rolling
// update: the new instance registers a UID-suffixed socket so the kubelet
// keeps talking to a prepared node across a driver restart, shrinking the
// prepare-unavailability window. (True zero-downtime maxSurge is not
// possible here — the pod is hostNetwork, so two instances can't share the
// node's ports; the DaemonSet uses maxUnavailable:1.) Requires kubelet
// >= 1.33, guaranteed by our >= 1.34 DRA floor.
func Start(ctx context.Context, client kubernetes.Interface, nodeName, podUID string, inv *Inventory, cdiDir string) (*kubeletplugin.Helper, error) {
	// The helper listens in this directory but does not create it.
	if err := os.MkdirAll(filepath.Join(kubeletplugin.KubeletPluginsDir, publisher.DriverName), 0o750); err != nil {
		return nil, fmt.Errorf("creating plugin socket dir: %w", err)
	}
	if err := gcOrphanedSpecs(ctx, client, cdiDir); err != nil {
		// GC is best-effort: a failed cleanup must not block prepare service.
		klog.FromContext(ctx).Error(err, "orphaned CDI spec GC failed; continuing")
	}
	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(publisher.DriverName),
		kubeletplugin.KubeClient(client),
		kubeletplugin.NodeName(nodeName),
	}
	if podUID != "" {
		opts = append(opts, kubeletplugin.RollingUpdate(types.UID(podUID)))
	}
	return kubeletplugin.Start(ctx, &Plugin{inv: inv, cdiDir: cdiDir}, opts...)
}

// PrepareResourceClaims writes one CDI spec per claim and maps each
// allocated device to its CDI ID. Per-claim failures are reported in the
// result map so one bad claim doesn't fail the batch.
func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		start := time.Now()
		res, reason := p.prepareClaim(ctx, claim)
		if res.Err != nil {
			observe.Prepare("error", reason, time.Since(start))
		} else {
			observe.Prepare("ok", "none", time.Since(start))
		}
		results[claim.UID] = res
	}
	return results, nil
}

// prepareClaim returns the result plus a bounded reason label for metrics:
// no_allocation | device_missing | foreign_only | cdi_write | none.
func (p *Plugin) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) (kubeletplugin.PrepareResult, string) {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name)}, "no_allocation"
	}
	var prepared []kubeletplugin.Device
	var cdiDevices []cdiDevice
	var amdgpu []int       // indices into cdiDevices
	var amdgpuIDs []string // matching sysfs unique_id values ("" when absent)
	// Node-global paths (/dev/kfd) appear in every amdgpu device's edits;
	// injected once per claim — some runtimes reject duplicate device nodes
	// in the merged OCI spec.
	seenNodes := map[string]bool{}
	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != publisher.DriverName {
			continue // another driver's result in a mixed claim (Phase 3 territory)
		}
		dev, ok := p.inv.lookup(alloc.Device)
		if !ok {
			// Allocation references a device the current probe doesn't see —
			// hardware went away between scheduling and prepare.
			return kubeletplugin.PrepareResult{Err: fmt.Errorf("allocated device %q not in current inventory", alloc.Device)}, "device_missing"
		}
		name := string(claim.UID) + "-" + alloc.Device
		edits := editsFor(dev)
		deduped := edits.DeviceNodes[:0]
		for _, n := range edits.DeviceNodes {
			if !seenNodes[n.Path] {
				seenNodes[n.Path] = true
				deduped = append(deduped, n)
			}
		}
		edits.DeviceNodes = deduped
		cdiDevices = append(cdiDevices, cdiDevice{Name: name, ContainerEdits: edits})
		if dev.Driver == "amdgpu" {
			amdgpu = append(amdgpu, len(cdiDevices)-1)
			amdgpuIDs = append(amdgpuIDs, dev.UniqueID)
		}
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: []string{qualifiedName(name)},
		})
	}
	p.addROCmVisibility(ctx, claim, cdiDevices, amdgpu, amdgpuIDs)
	if len(prepared) == 0 {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no %s allocation results", claim.Namespace, claim.Name, publisher.DriverName)}, "foreign_only"
	}
	if err := writeSpec(p.cdiDir, string(claim.UID), cdiDevices); err != nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("writing CDI spec: %w", err)}, "cdi_write"
	}
	klog.FromContext(ctx).V(2).Info("prepared claim", "claim", klog.KObj(claim), "devices", len(prepared))
	return kubeletplugin.PrepareResult{Devices: prepared}, "none"
}

// addROCmVisibility scopes ROCm compute to the claim's own GPUs. /dev/kfd is
// node-global: on a multi-GPU AMD node, injecting it lets KFD enumerate every
// AMD GPU while only the allocated render nodes are present — an isolation
// leak, and some ROCm versions abort when they can enumerate a GPU whose
// render node they cannot open. ROCR_VISIBLE_DEVICES=<GPU-uuid,…> closes it.
//
// Deliberately conservative: single-amdgpu nodes need no isolation and get no
// env (a wrong value would break the working happy path), and nodes whose
// ASICs expose no unique_id (common on APUs) log the gap instead of guessing.
// The identical joined value goes on EVERY amdgpu device's edits, so the
// runtime's env merge is order-independent.
func (p *Plugin) addROCmVisibility(ctx context.Context, claim *resourceapi.ResourceClaim, cdiDevices []cdiDevice, amdgpu []int, ids []string) {
	if len(amdgpu) == 0 || p.inv.driverCount("amdgpu") <= 1 {
		return
	}
	for _, id := range ids {
		if id == "" {
			klog.FromContext(ctx).V(2).Info("multi-GPU amdgpu node without sysfs unique_id; skipping ROCR_VISIBLE_DEVICES isolation",
				"claim", klog.KObj(claim))
			return
		}
	}
	env := "ROCR_VISIBLE_DEVICES=GPU-" + strings.Join(ids, ",GPU-")
	for _, i := range amdgpu {
		cdiDevices[i].ContainerEdits.Env = append(cdiDevices[i].ContainerEdits.Env, env)
	}
}

// editsFor maps a probed device to its container edits: every /dev node the
// device needs, plus LLMFIT_* env identifying what was granted. amdgpu
// compute additionally requires the node-global /dev/kfd (KFD is how ROCm
// reaches the GPU; the render node alone only covers Vulkan).
//
// LLMFIT_DEVICE/_KIND/_RENDER_NODE describe *a* device — with multiple
// devices in one claim the runtime merges the edits and the last write
// wins, so LLMFIT_DEVICE_<NAME> is the per-device key that survives the
// merge (value: the device node consumers should open, or the kind).
func editsFor(d probe.Device) containerEdits {
	perDevice := d.RenderNode
	if perDevice == "" {
		perDevice = d.DevNode
	}
	if perDevice == "" {
		perDevice = string(d.Kind)
	}
	edits := containerEdits{
		Env: []string{
			"LLMFIT_DEVICE=" + d.Name(),
			"LLMFIT_DEVICE_KIND=" + string(d.Kind),
			// PCI-derived names contain dashes, which are invalid in env
			// var names — map to underscores.
			"LLMFIT_DEVICE_" + strings.ToUpper(strings.ReplaceAll(d.Name(), "-", "_")) + "=" + perDevice,
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
	if d.Kind == probe.KindNIC && d.DevNode != "" {
		// Verbs alone can't establish connections: librdmacm needs the
		// node-global /dev/infiniband/rdma_cm beside the device's uverbs
		// node (deduped per claim like /dev/kfd). LLMFIT_NETDEV names the
		// paired netdev so RoCE consumers can bind the right interface.
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: "/dev/infiniband/rdma_cm"})
		if d.NetDev != "" {
			edits.Env = append(edits.Env, "LLMFIT_NETDEV="+d.NetDev)
		}
	}
	return edits
}

// HandleError receives background failures from the helper's gRPC servers.
// The helper's contract distinguishes recoverable errors (log, kubelet
// retries) from fatal ones (a dead DRA/registrar server that will never
// serve again). A fatal error must not leave a zombie pod that logs while
// 'Running': exit and let the DaemonSet restart us with fresh sockets.
func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		klog.FromContext(ctx).Error(err, msg)
		return
	}
	klog.FromContext(ctx).Error(err, msg+" (fatal; exiting for DaemonSet restart)")
	klog.Flush()
	os.Exit(1)
}

// gcOrphanedSpecs removes CDI spec files whose ResourceClaim no longer
// exists — the leak window is a crash between writeSpec and the kubelet
// recording the prepare, or kubelet checkpoint loss: nothing would ever
// call Unprepare for that UID again. Runs once at startup; live claims'
// specs are never touched.
func gcOrphanedSpecs(ctx context.Context, client kubernetes.Interface, cdiDir string) error {
	uids, err := specUIDs(cdiDir)
	if err != nil || len(uids) == 0 {
		return err
	}
	claims, err := client.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing resourceclaims: %w", err)
	}
	live := make(map[string]bool, len(claims.Items))
	for _, c := range claims.Items {
		live[string(c.UID)] = true
	}
	for _, uid := range uids {
		if live[uid] {
			continue
		}
		if err := removeSpec(cdiDir, uid); err != nil {
			klog.FromContext(ctx).Error(err, "removing orphaned CDI spec", "uid", uid)
			continue
		}
		observe.CDIOrphanRemoved()
		klog.FromContext(ctx).Info("removed orphaned CDI spec", "uid", uid)
	}
	return nil
}

// UnprepareResourceClaims removes each claim's CDI spec. The kubelet may call
// this for claims a previous driver instance prepared, so missing files are
// success, and errors are reported per claim.
func (p *Plugin) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		start := time.Now()
		err := removeSpec(p.cdiDir, string(claim.UID))
		if err != nil {
			observe.Unprepare("error", time.Since(start))
		} else {
			observe.Unprepare("ok", time.Since(start))
		}
		results[claim.UID] = err
	}
	return results, nil
}
