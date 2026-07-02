// llmfit-dra publishes this node's accelerator inventory (probe ⋈ index) as
// DRA ResourceSlices under the llmfit.ai driver. Runs as a DaemonSet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/sympozium-ai/llmfit-dra/internal/hotplug"
	"github.com/sympozium-ai/llmfit-dra/internal/index"
	"github.com/sympozium-ai/llmfit-dra/internal/llmfit"
	"github.com/sympozium-ai/llmfit-dra/internal/nodeplugin"
	"github.com/sympozium-ai/llmfit-dra/internal/probe"
	"github.com/sympozium-ai/llmfit-dra/internal/publisher"
)

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster)")
		nodeName   = flag.String("node-name", os.Getenv("NODE_NAME"), "node this agent runs on (default: NODE_NAME env)")
		sysRoot    = flag.String("sys-root", envOr("LLMFIT_SYS_ROOT", "/sys"), "sysfs root (mount host /sys here in a container)")
		procRoot   = flag.String("proc-root", envOr("LLMFIT_PROC_ROOT", "/proc"), "procfs root")
		llmfitBin  = flag.String("llmfit-bin", envOr("LLMFIT_BIN", "llmfit"), "llmfit binary for capability assessment; empty disables")
		interval   = flag.Duration("probe-interval", 60*time.Second, "re-probe cadence; slices update only when inventory changes")
		nodePlugin = flag.Bool("kubelet-plugin", true, "serve the kubelet DRA plugin (NodePrepareResources → CDI); disable for publish-only")
		cdiDir     = flag.String("cdi-dir", "/var/run/cdi", "dynamic CDI spec directory (host mount)")
		taints     = flag.Bool("publish-taints", false, "taint unhealthy devices NoSchedule (requires the DRADeviceTaints feature gate)")
	)
	klog.InitFlags(nil)
	flag.Parse()

	if err := run(*kubeconfig, *nodeName, *sysRoot, *procRoot, *llmfitBin, *interval, *nodePlugin, *cdiDir, *taints); err != nil {
		klog.ErrorS(err, "llmfit-dra failed")
		os.Exit(1)
	}
}

func run(kubeconfig, nodeName, sysRoot, procRoot, llmfitBin string, interval time.Duration, nodePlugin bool, cdiDir string, taints bool) error {
	if nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME is required")
	}

	cfg, err := buildConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("building kube config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building kube client: %w", err)
	}

	idx, err := index.Load()
	if err != nil {
		return err
	}
	klog.InfoS("capability index loaded", "entries", idx.Len())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	prober := probe.New(sysRoot, procRoot)
	inv := nodeplugin.NewInventory()
	opts := publisher.Options{Taints: taints}
	devices, resources, err := desiredState(ctx, prober, idx, llmfitBin, nodeName, inv, opts)
	if err != nil {
		return err
	}
	klog.InfoS("initial probe complete", "devices", len(devices), "node", nodeName)

	controller, err := publisher.Start(ctx, client, nodeName, resources)
	if err != nil {
		return fmt.Errorf("starting resourceslice controller: %w", err)
	}
	defer controller.Stop()

	if nodePlugin {
		helper, err := nodeplugin.Start(ctx, client, nodeName, inv, cdiDir)
		if err != nil {
			return fmt.Errorf("starting kubelet plugin: %w", err)
		}
		defer helper.Stop()
		klog.InfoS("kubelet DRA plugin registered", "cdiDir", cdiDir)
	}

	// Event-driven re-probe: kernel uevents on the drm/accel/pci subsystems
	// (hot-attach, driver bind/unbind, error events) trigger an immediate
	// walk; the ticker remains as the reconciliation floor. Update is a
	// no-op server-side when nothing changed because the helper diffs
	// desired vs published state.
	uevents, err := hotplug.Listen(ctx, 2*time.Second)
	if err != nil {
		klog.ErrorS(err, "uevent listener unavailable (needs hostNetwork); ticker-only probing")
		uevents = nil // nil channel: select arm never fires
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	prev := devices
	for {
		select {
		case <-ctx.Done():
			klog.InfoS("shutting down")
			return nil
		case <-uevents:
			klog.InfoS("uevent-triggered re-probe")
		case <-ticker.C:
		}
		devices, resources, err = desiredState(ctx, prober, idx, llmfitBin, nodeName, inv, opts)
		if err != nil {
			klog.ErrorS(err, "re-probe failed; keeping previous inventory")
			continue
		}
		if !resourceslice.DevicesDeepEqual(prev, devices) {
			klog.InfoS("inventory changed; updating resourceslices", "devices", len(devices))
			prev = devices
		}
		// Always push desired state, changed or not: Update re-queues a
		// pool sync, making the publisher self-healing when slices are
		// deleted externally (the helper's own delete-event path can miss
		// recreation when a delete lands inside its mutation-cache TTL).
		// An unchanged sync issues no API writes, so this is cheap.
		controller.Update(resources)
	}
}

func desiredState(ctx context.Context, prober *probe.Prober, idx *index.Index, llmfitBin, nodeName string, inv *nodeplugin.Inventory, opts publisher.Options) ([]resourceapi.Device, *resourceslice.DriverResources, error) {
	probed, err := prober.Walk()
	if err != nil {
		return nil, nil, fmt.Errorf("device tree walk: %w", err)
	}
	// Keep the kubelet plugin's view in lockstep with what we publish:
	// Prepare joins allocation results back to /dev nodes via this snapshot.
	inv.Set(probed)
	ram, err := prober.MemTotalBytes()
	if err != nil {
		return nil, nil, fmt.Errorf("reading system RAM: %w", err)
	}
	// llmfit is the preferred capability source; degrade to the embedded
	// index rather than failing the whole publish if it breaks.
	var sys *llmfit.System
	if llmfitBin != "" {
		sys, err = llmfit.Detect(ctx, llmfitBin)
		if err != nil {
			klog.ErrorS(err, "llmfit detection failed; falling back to embedded index")
			sys = nil
		} else {
			klog.V(2).InfoS("llmfit capability assessment", "cpu", sys.CPUName, "gpus", len(sys.GPUs))
		}
	}
	devices := publisher.BuildDevices(probed, idx, ram, sys, opts)
	return devices, publisher.BuildResources(nodeName, devices), nil
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
