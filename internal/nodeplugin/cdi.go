// CDI spec generation. We write the (small, stable) CDI JSON directly rather
// than pulling in the container-device-interface module: the runtime
// (containerd ≥ 1.7 / CRI-O) is the consumer and validator, and one file per
// claim in the dynamic spec dir doubles as the plugin's on-disk state —
// Unprepare is "remove the file", restarts need no checkpoint.
package nodeplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// cdiVersion 0.6.0 is the floor for containerd 1.7+; we use nothing newer.
	cdiVersion = "0.6.0"
	// cdiKind qualifies every device we edit into a container:
	// llmfit.ai/device=<claimUID>-<deviceName>.
	cdiKind = "llmfit.ai/device"
)

type cdiSpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []cdiDevice `json:"devices"`
}

type cdiDevice struct {
	Name           string         `json:"name"`
	ContainerEdits containerEdits `json:"containerEdits"`
}

type containerEdits struct {
	Env         []string        `json:"env,omitempty"`
	DeviceNodes []cdiDeviceNode `json:"deviceNodes,omitempty"`
}

type cdiDeviceNode struct {
	Path string `json:"path"`
}

// qualifiedName is the ID handed back to the kubelet, e.g.
// "llmfit.ai/device=0ba5…-gpu0".
func qualifiedName(deviceName string) string {
	return cdiKind + "=" + deviceName
}

// specPath is the per-claim CDI spec file inside dir.
func specPath(dir, claimUID string) string {
	return filepath.Join(dir, "llmfit.ai-"+claimUID+".json")
}

// writeSpec atomically writes the claim's CDI spec (temp file + rename, so
// the runtime's fsnotify watcher never reads a partial spec).
func writeSpec(dir, claimUID string, devices []cdiDevice) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating CDI dir: %w", err)
	}
	data, err := json.MarshalIndent(cdiSpec{
		CDIVersion: cdiVersion,
		Kind:       cdiKind,
		Devices:    devices,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".llmfit-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), specPath(dir, claimUID))
}

// removeSpec deletes the claim's CDI spec; already-gone is success
// (Unprepare must be idempotent and may run after a driver restart or on a
// claim a previous instance prepared).
func removeSpec(dir, claimUID string) error {
	err := os.Remove(specPath(dir, claimUID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
