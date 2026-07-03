// Package modelclaim reconciles ModelClaim objects (llmfit.ai/v1alpha1) into
// same-named ResourceClaimTemplates. See docs/design/modelclaim.md.
package modelclaim

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// Bounds is the resolver's answer: the literal numbers the fit CEL needs,
// as emitted by `llmfit --json claim` (upstream M0, llmfit ≥ 0.9.37).
type Bounds struct {
	Model           string  `json:"model"`
	ClaimName       string  `json:"claimName"`
	Quant           string  `json:"quant"`
	WeightsGb       float64 `json:"weightsGb"`
	MemoryGi        uint64  `json:"memoryGi"`
	MinBandwidthGBs uint64  `json:"minBandwidthGBs"`
	MinTps          float64 `json:"minTps"`
	EfficiencyPct   uint32  `json:"efficiencyPct"`
	ResolverVersion string  `json:"resolverVersion"`
}

// Resolver turns a model reference into fit bounds.
type Resolver interface {
	Resolve(ctx context.Context, model string, minTps float64, quant string, efficiencyPct int64) (*Bounds, error)
}

// ExecResolver shells out to the llmfit binary shipped in the driver image —
// the same degradation-free path the CLI uses; the controller pod always has
// the binary, so no API transport is needed (design: M3 may add one).
type ExecResolver struct {
	Bin     string
	Timeout time.Duration
}

func (r *ExecResolver) Resolve(ctx context.Context, model string, minTps float64, quant string, efficiencyPct int64) (*Bounds, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--json", "claim", model,
		"--min-tps", strconv.FormatFloat(minTps, 'f', -1, 64)}
	if quant != "" {
		args = append(args, "--quant", quant)
	}
	if efficiencyPct > 0 {
		args = append(args, "--efficiency", strconv.FormatInt(efficiencyPct, 10))
	}

	out, err := exec.CommandContext(ctx, r.Bin, args...).Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			detail = ": " + string(ee.Stderr)
		}
		return nil, fmt.Errorf("llmfit claim %q%s (%w)", model, detail, err)
	}
	var b Bounds
	if err := json.Unmarshal(out, &b); err != nil {
		return nil, fmt.Errorf("parsing llmfit claim output: %w", err)
	}
	if b.MemoryGi == 0 && b.MinBandwidthGBs == 0 {
		return nil, fmt.Errorf("llmfit claim returned empty bounds for %q", model)
	}
	return &b, nil
}
