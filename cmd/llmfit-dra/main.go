// llmfit-dra publishes this node's accelerator inventory (probe ⋈ index) as
// DRA ResourceSlices under the llmfit.ai driver. Runs as a DaemonSet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/sympozium-ai/llmfit-dra/internal/hotplug"
	"github.com/sympozium-ai/llmfit-dra/internal/index"
	"github.com/sympozium-ai/llmfit-dra/internal/llmfit"
	"github.com/sympozium-ai/llmfit-dra/internal/modelclaim"
	"github.com/sympozium-ai/llmfit-dra/internal/nodeplugin"
	"github.com/sympozium-ai/llmfit-dra/internal/observe"
	"github.com/sympozium-ai/llmfit-dra/internal/probe"
	"github.com/sympozium-ai/llmfit-dra/internal/publisher"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster)")
		nodeName    = flag.String("node-name", os.Getenv("NODE_NAME"), "node this agent runs on (default: NODE_NAME env)")
		sysRoot     = flag.String("sys-root", envOr("LLMFIT_SYS_ROOT", "/sys"), "sysfs root (mount host /sys here in a container)")
		procRoot    = flag.String("proc-root", envOr("LLMFIT_PROC_ROOT", "/proc"), "procfs root")
		llmfitBin   = flag.String("llmfit-bin", envOr("LLMFIT_BIN", "llmfit"), "llmfit binary for capability assessment (exec fallback); empty disables")
		llmfitURL   = flag.String("llmfit-url", envOr("LLMFIT_URL", ""), "llmfit serve API (unix:///path.sock or http://…); preferred over exec when set")
		interval    = flag.Duration("probe-interval", 60*time.Second, "re-probe cadence; slices update only when inventory changes")
		nodePlugin  = flag.Bool("kubelet-plugin", true, "serve the kubelet DRA plugin (NodePrepareResources → CDI); disable for publish-only")
		cdiDir      = flag.String("cdi-dir", "/var/run/cdi", "dynamic CDI spec directory (host mount)")
		taints      = flag.Bool("publish-taints", false, "taint unhealthy devices NoSchedule (requires the DRADeviceTaints feature gate)")
		vendors     = flag.String("vendor-drivers", publisher.DefaultVendorDrivers, "DRA drivers that own GPU allocation; their presence on this node demotes our GPUs to fitness-only (empty disables)")
		metricsAddr = flag.String("metrics-addr", envOr("METRICS_ADDR", ":9099"), "address for the /metrics, /healthz and /readyz server")
		controller  = flag.Bool("controller", false, "run the cluster-scoped ModelClaim controller instead of the per-node agent (Deployment, not DaemonSet)")
	)
	klog.InitFlags(nil)
	flag.Parse()

	if *controller {
		if err := runController(*kubeconfig, *llmfitBin, *metricsAddr); err != nil {
			klog.ErrorS(err, "llmfit-dra modelclaim controller failed")
			os.Exit(1)
		}
		return
	}

	if err := run(*kubeconfig, *nodeName, *sysRoot, *procRoot, *llmfitBin, *llmfitURL, *interval, *nodePlugin, *cdiDir, *taints, *vendors, *metricsAddr); err != nil {
		klog.ErrorS(err, "llmfit-dra failed")
		os.Exit(1)
	}
}

// runController is the --controller mode: cluster-scoped ModelClaim →
// ResourceClaimTemplate reconciliation. Deliberately not part of the
// DaemonSet run: it needs none of the host privileges (hostNetwork,
// NET_ADMIN, /sys) the per-node agent holds.
func runController(kubeconfig, llmfitBin, metricsAddr string) error {
	cfg, err := buildConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("building kube config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Same observability contract as the node agent: /metrics + probes, and
	// a listen failure takes the pod down. Readiness flips after cache sync;
	// liveness rides worker beats plus an idle tick (see Controller.Run).
	health := observe.NewHealth(2 * time.Minute)
	go func() {
		if err := health.Serve(ctx, metricsAddr); err != nil {
			klog.ErrorS(err, "metrics/health server failed; exiting")
			os.Exit(1)
		}
	}()

	resolver := &modelclaim.ExecResolver{Bin: llmfitBin}
	c := modelclaim.New(dyn, client, resolver)
	c.Health = health
	return c.Run(ctx, 2)
}

func run(kubeconfig, nodeName, sysRoot, procRoot, llmfitBin, llmfitURL string, interval time.Duration, nodePlugin bool, cdiDir string, taints bool, vendorFlag, metricsAddr string) error {
	if nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME is required")
	}
	if interval < time.Second {
		// time.NewTicker(0) panics, and NewHealth(3*interval) would make
		// liveness unsatisfiable for tiny intervals.
		return fmt.Errorf("--probe-interval must be at least 1s, got %s", interval)
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

	// Liveness fails if the reconcile loop hasn't completed a cycle within
	// three intervals — a hung loop becomes a failing probe, not a silent
	// 1/1-Running node. Observability is not optional here: a listen failure
	// takes the pod down for a restart.
	health := observe.NewHealth(3 * interval)
	go func() {
		if err := health.Serve(ctx, metricsAddr); err != nil {
			klog.ErrorS(err, "metrics/health server failed; exiting")
			os.Exit(1)
		}
	}()

	prober := probe.New(sysRoot, procRoot)
	inv := nodeplugin.NewInventory()
	// Capability source: serve API preferred, exec fallback, last-known-good
	// within 10 minutes so a transient llmfit failure cannot flap published
	// attributes (source llmfit -> index churn).
	detector, err := llmfit.NewDetector(llmfitURL, llmfitBin, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("configuring llmfit source: %w", err)
	}
	vendorDrivers := publisher.ParseVendorDrivers(vendorFlag)
	opts := publisher.Options{Taints: taints}
	// refreshOpts re-evaluates per-cycle facts that live outside sysfs.
	var prevForeign []string
	refreshOpts := func() {
		vm, foreign, err := publisher.VendorGPUsPresent(ctx, client, nodeName, vendorDrivers)
		if err != nil {
			observe.ProbeError("coexist")
			klog.ErrorS(err, "vendor coexistence check failed; keeping previous state")
			return
		}
		if vm != opts.VendorManagedGPUs {
			klog.InfoS("vendor GPU driver presence changed; GPUs demoted to fitness-only", "vendorManaged", vm)
		}
		if !slices.Equal(foreign, prevForeign) {
			// A DRA driver we don't recognize publishes for this node: if it
			// owns GPUs, our coexistence list has a gap — surface it rather
			// than silently double-booking.
			klog.InfoS("unrecognized DRA drivers publish for this node (no coexistence demotion applied)", "drivers", foreign)
			prevForeign = foreign
		}
		opts.VendorManagedGPUs = vm
	}
	refreshOpts()
	devices, resources, err := desiredState(ctx, prober, idx, detector, nodeName, inv, opts)
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
		helper, err := nodeplugin.Start(ctx, client, nodeName, os.Getenv("POD_UID"), inv, cdiDir)
		if err != nil {
			return fmt.Errorf("starting kubelet plugin: %w", err)
		}
		defer helper.Stop()
		klog.InfoS("kubelet DRA plugin registered", "cdiDir", cdiDir)
	}
	// Publisher and (optional) plugin are up: report ready. Liveness (Beat)
	// tracks each subsequent successful cycle.
	health.Ready()
	health.Beat()

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
	observe.HotplugListener(uevents != nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	prev := devices
	logInventory(devices)
	for {
		select {
		case <-ctx.Done():
			klog.InfoS("shutting down")
			return nil
		case _, ok := <-uevents:
			if !ok {
				// Listener died mid-run (fatal read error): degrade to
				// ticker-only, visibly.
				uevents = nil
				observe.HotplugListener(false)
				klog.ErrorS(nil, "hotplug listener stopped; ticker-only probing")
				continue
			}
			observe.HotplugWakeup()
			klog.InfoS("uevent-triggered re-probe")
		case <-ticker.C:
		}
		start := time.Now()
		refreshOpts()
		devices, resources, err = desiredState(ctx, prober, idx, detector, nodeName, inv, opts)
		if err != nil {
			klog.ErrorS(err, "re-probe failed; keeping previous inventory")
			continue // a failed cycle does NOT Beat: liveness reflects real progress
		}
		changed := !resourceslice.DevicesDeepEqual(prev, devices)
		if changed {
			klog.InfoS("inventory changed; updating resourceslices", "devices", len(devices))
			logInventory(devices)
			prev = devices
		}
		// Always push desired state, changed or not: Update re-queues a
		// pool sync, making the publisher self-healing when slices are
		// deleted externally (the helper's own delete-event path can miss
		// recreation when a delete lands inside its mutation-cache TTL).
		// An unchanged sync issues no API writes, so this is cheap.
		controller.Update(resources)
		observe.ObserveProbe(time.Since(start), changed)
		health.Beat()
	}
}

func desiredState(ctx context.Context, prober *probe.Prober, idx *index.Index, detector *llmfit.Detector, nodeName string, inv *nodeplugin.Inventory, opts publisher.Options) ([]resourceapi.Device, *resourceslice.DriverResources, error) {
	probed, err := prober.Walk()
	if err != nil {
		observe.ProbeError("walk")
		return nil, nil, fmt.Errorf("device tree walk: %w", err)
	}
	// Keep the kubelet plugin's view in lockstep with what we publish:
	// Prepare joins allocation results back to /dev nodes via this snapshot.
	inv.Set(probed)
	ram, err := prober.MemTotalBytes()
	if err != nil {
		observe.ProbeError("meminfo")
		return nil, nil, fmt.Errorf("reading system RAM: %w", err)
	}
	// llmfit is the preferred capability source; degrade to the embedded
	// index rather than failing the whole publish if it breaks.
	sys, err := detector.Detect(ctx)
	if err != nil {
		observe.ProbeError("detect")
		klog.ErrorS(err, "llmfit detection failed; falling back to embedded index")
		sys = nil
		observe.SetCapabilitySource("index")
	} else {
		klog.V(2).InfoS("llmfit capability assessment", "cpu", sys.CPUName, "gpus", len(sys.GPUs))
		observe.SetCapabilitySource(detector.Transport())
	}
	devices := publisher.BuildDevices(probed, idx, ram, sys, opts)
	observe.SetInventory(deviceInfos(devices))
	return devices, publisher.BuildResources(nodeName, devices), nil
}

// attrStr/attrBool read published device attributes for the gauges and the
// inventory log — the attributes are the single source of truth for what was
// actually published.
func attrStr(d resourceapi.Device, key string) string {
	if a, ok := d.Attributes[resourceapi.QualifiedName(key)]; ok && a.StringValue != nil {
		return *a.StringValue
	}
	return ""
}

func attrBool(d resourceapi.Device, key string) bool {
	if a, ok := d.Attributes[resourceapi.QualifiedName(key)]; ok && a.BoolValue != nil {
		return *a.BoolValue
	}
	return false
}

func deviceInfos(devices []resourceapi.Device) []observe.DeviceInfo {
	infos := make([]observe.DeviceInfo, 0, len(devices))
	for _, d := range devices {
		infos = append(infos, observe.DeviceInfo{
			Kind:          attrStr(d, "kind"),
			Vendor:        attrStr(d, "vendor"),
			Driver:        attrStr(d, "driver"),
			Healthy:       attrBool(d, "healthy"),
			VendorManaged: attrBool(d, "vendorManaged"),
		})
	}
	return infos
}

// logInventory prints one line per published device — "devices: 5" alone
// forces sysfs spelunking to find out WHAT changed on a node.
func logInventory(devices []resourceapi.Device) {
	for _, d := range devices {
		mem := ""
		if c, ok := d.Capacity["memory"]; ok {
			mem = c.Value.String()
		}
		bw := int64(0)
		if a, ok := d.Attributes["memoryBandwidthGBs"]; ok && a.IntValue != nil {
			bw = *a.IntValue
		}
		klog.InfoS("published device",
			"name", d.Name,
			"kind", attrStr(d, "kind"),
			"vendor", attrStr(d, "vendor"),
			"driver", attrStr(d, "driver"),
			"model", attrStr(d, "model"),
			"source", attrStr(d, "source"),
			"memory", mem,
			"bandwidthGBs", bw,
			"healthy", attrBool(d, "healthy"),
			"vendorManaged", attrBool(d, "vendorManaged"),
		)
	}
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
